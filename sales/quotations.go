package sales

import (
	"context"
	"fmt"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/search"
)

// Quotations: drafts, sending, cancelling, and accepting one into an order draft (prices kept).

type quotationIn struct {
	LegalEntityID string   `json:"legal_entity_id"`
	CustomerID    string   `json:"customer_id"`
	Currency      string   `json:"currency"`
	OwnerID       *string  `json:"owner_id"`
	DepartmentID  *string  `json:"department_id"`
	ValidUntil    *string  `json:"valid_until"`
	Lines         []lineIn `json:"lines"`
}

func (m *Module) createQuotation(c *httpx.Context) error {
	var in quotationIn
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.ValidUntil != nil && !dateForm.MatchString(*in.ValidUntil) {
		return invalid("valid_until", "valid_until is YYYY-MM-DD")
	}
	ctx, actor, h, ev, err := m.prepare(c, OpQuoteEdit, in.LegalEntityID, in.CustomerID, in.Currency, in.OwnerID,
		in.DepartmentID, in.Lines)
	if err != nil {
		return err
	}
	today, err := bizdate.Today(h.le.Timezone)
	if err != nil {
		return err
	}
	number, err := m.number(ctx, "QT", h.le, today)
	if err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		lines, err := buildLines(ctx, tx, h, today, in.Lines, ev, nil)
		if err != nil {
			return idem.Outcome{}, err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO quotation (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, valid_until, owner_id, department_id, department_path, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, number, h.le.ID, h.cust.ID,
			h.cust.Name, h.cust.TaxID, h.currency, in.ValidUntil, h.owner, h.deptID, h.deptPath,
			search.Text(number, h.cust.Name), search.Version); err != nil {
			return idem.Outcome{}, err
		}
		if err := writeLines(ctx, tx, "quotation", id, lines); err != nil {
			return idem.Outcome{}, err
		}
		q, err := m.emitQuotation(ctx, tx, id)
		if err != nil {
			return idem.Outcome{}, err
		}
		return idResult(id), m.change(ctx, tx, actor, OpQuoteEdit, Quotations, id, q)
	})
	if err != nil {
		return err
	}
	q, err := m.quotation(ctx, resultID(out))
	if err != nil {
		return err
	}
	return c.JSON(200, q)
}

func (m *Module) getQuotation(c *httpx.Context) error {
	q, err := m.quotation(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpQuoteRead, docFacts(q.LegalEntityID, q.DepartmentPath, q.OwnerID)); err != nil {
		return err
	}
	return c.JSON(200, q)
}

func (m *Module) updateQuotation(c *httpx.Context) error {
	var in patchIn
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.BusinessDate != nil {
		return invalid("business_date", "a quotation has no business date")
	}
	if in.ValidUntil != nil && !dateForm.MatchString(*in.ValidUntil) {
		return invalid("valid_until", "valid_until is YYYY-MM-DD")
	}
	cur, err := m.quotation(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, h, ev, err := m.patchHead(c, OpQuoteEdit, &in, cur.LegalEntityID, cur.Currency, cur.Customer.PartnerID,
		cur.OwnerID, cur.DepartmentID, cur.DepartmentPath)
	if err != nil {
		return err
	}
	var out Quotation
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		q, err := getQuotation(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if q.State != "draft" {
			return errState.New("only a draft quotation changes; this one is " + q.State)
		}
		if in.Version != nil && *in.Version != q.Version {
			return errVersion.New(fmt.Sprintf("the quotation is at version %d", q.Version))
		}
		if in.Lines != nil {
			today, err := bizdate.Today(h.le.Timezone)
			if err != nil {
				return err
			}
			lines, err := buildLines(ctx, tx, h, today, in.Lines, ev, q.Lines)
			if err != nil {
				return err
			}
			if err := writeLines(ctx, tx, "quotation", q.ID, lines); err != nil {
				return err
			}
		}
		validUntil := q.ValidUntil
		if in.ValidUntil != nil {
			validUntil = in.ValidUntil
		}
		if _, err := tx.Exec(ctx, `UPDATE quotation SET owner_id = $2, department_id = $3, department_path = $4,
			valid_until = $5::date, version = version + 1, updated_at = now() WHERE id = $1`,
			q.ID, h.owner, h.deptID, h.deptPath, validUntil); err != nil {
			return err
		}
		if out, err = m.emitQuotation(ctx, tx, q.ID); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpQuoteEdit, Quotations, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// quotationAction sends or cancels a quotation; repeating the action on a quotation already in its state answers it
// as it is.
func (m *Module) quotationAction(action string) httpx.Handler {
	from := map[string][]string{"send": {"draft"}, "cancel": {"draft", "sent"}}[action]
	to := map[string]string{"send": "sent", "cancel": "cancelled"}[action]
	return func(c *httpx.Context) error {
		cur, err := m.quotation(c.Ctx(), c.Param("id"))
		if err != nil {
			return err
		}
		ctx, actor, err := check(c, OpQuoteEdit, docFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID))
		if err != nil {
			return err
		}
		var out Quotation
		err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
			q, err := getQuotation(ctx, tx, cur.ID, true)
			if err != nil {
				return err
			}
			if q.State == to {
				out = q
				return nil
			}
			if !contains(from, q.State) {
				return errState.New("a " + q.State + " quotation cannot be " + to)
			}
			if _, err := tx.Exec(ctx, `UPDATE quotation SET state = $2, version = version + 1, updated_at = now()
				WHERE id = $1`, q.ID, to); err != nil {
				return err
			}
			if out, err = m.emitQuotation(ctx, tx, q.ID); err != nil {
				return err
			}
			return m.change(ctx, tx, actor, OpQuoteEdit, Quotations, out.ID, out)
		})
		if err != nil {
			return err
		}
		return c.JSON(200, out)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// acceptQuotation turns a sent quotation into an order draft with the same lines and prices; accepting it again
// answers the quotation with its order.
func (m *Module) acceptQuotation(c *httpx.Context) error {
	cur, err := m.quotation(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	facts := docFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID)
	ctx, actor, err := check(c, OpQuoteEdit, facts)
	if err != nil {
		return err
	}
	if cur.State == "accepted" {
		return c.JSON(200, cur)
	}
	if _, _, err := check(c, OpOrderEdit, facts); err != nil {
		return err
	}
	le, err := m.legalEntityOf(ctx, cur.LegalEntityID)
	if err != nil {
		return err
	}
	today, err := bizdate.Today(le.Timezone)
	if err != nil {
		return err
	}
	if cur.ValidUntil != nil && *cur.ValidUntil < today {
		return errState.New("the quotation was valid until " + *cur.ValidUntil)
	}
	number, err := m.number(ctx, "SO", le, today)
	if err != nil {
		return err
	}
	var out Quotation
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		q, err := getQuotation(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if q.State == "accepted" {
			out = q
			return nil
		}
		if q.State != "sent" {
			return errState.New("only a sent quotation is accepted; this one is " + q.State)
		}
		orderID := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO sales_order (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, business_date, owner_id, department_id, department_path, quotation_id, search,
			search_version) SELECT $2, $3, legal_entity_id, customer_id, customer_name, customer_tax_id, currency,
			$4::date, owner_id, department_id, department_path, id, $5, $6 FROM quotation WHERE id = $1`,
			q.ID, orderID, number, today, search.Text(number, q.Customer.Name), search.Version); err != nil {
			return err
		}
		olds, news := make([]string, len(q.Lines)), make([]string, len(q.Lines))
		for i, l := range q.Lines {
			olds[i], news[i] = l.ID, ids.New().String()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO order_line (id, order_id, line_no, kind, product_id, product_code,
			product_name, base_unit, quantity, unit, conversion_factor, base_quantity, warehouse_id, unit_price, price_source,
			price_list_item_id, tax_rate, net, tax) SELECT n.new_id, $1, l.line_no, l.kind, l.product_id, l.product_code,
			l.product_name, l.base_unit, l.quantity, l.unit, l.conversion_factor, l.base_quantity, l.warehouse_id,
			l.unit_price, l.price_source, l.price_list_item_id, l.tax_rate, l.net, l.tax
			FROM quotation_line l JOIN unnest($2::uuid[], $3::uuid[]) AS n(old_id, new_id) ON n.old_id = l.id`,
			orderID, olds, news); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sales_order SET net = $2::numeric, tax = $3::numeric WHERE id = $1`,
			orderID, q.Totals.Net, q.Totals.Tax); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quotation SET state = 'accepted', order_id = $2, version = version + 1,
			updated_at = now() WHERE id = $1`, q.ID, orderID); err != nil {
			return err
		}
		o, err := m.emitOrder(ctx, tx, orderID)
		if err != nil {
			return err
		}
		if err := m.change(ctx, tx, actor, OpOrderEdit, Orders, o.ID, o); err != nil {
			return err
		}
		if out, err = m.emitQuotation(ctx, tx, q.ID); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpQuoteEdit, Quotations, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}
