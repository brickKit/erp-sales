package quotations

import (
	"context"
	"fmt"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	documents "github.com/brickKit/erp-sales/v3/internal/documents"
	masterdata "github.com/brickKit/erp-sales/v3/internal/masterdata"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
	"github.com/brickKit/be-sdk-go/v3/search"
)

// Quotations: drafts, sending, cancelling, and accepting one into an order draft (prices kept).

type quotationIn struct {
	LegalEntityID string             `json:"legal_entity_id"`
	CustomerID    string             `json:"customer_id"`
	Currency      string             `json:"currency"`
	OwnerID       *string            `json:"owner_id"`
	DepartmentID  *string            `json:"department_id"`
	ValidUntil    *string            `json:"valid_until"`
	Lines         []documents.LineIn `json:"lines"`
}

func CreateQuotation(m *core.Base, c *httpx.Context) error {
	var in quotationIn
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.ValidUntil != nil && !core.DateForm.MatchString(*in.ValidUntil) {
		return core.Invalid("valid_until", "valid_until is YYYY-MM-DD")
	}
	ctx, actor, h, ev, err := documents.Prepare(m, c, core.OpQuoteEdit, in.LegalEntityID, in.CustomerID, in.Currency, in.OwnerID,
		in.DepartmentID, in.Lines)
	if err != nil {
		return err
	}
	today, err := bizdate.Today(h.Le.Timezone)
	if err != nil {
		return err
	}
	number, err := documents.Number(m, ctx, "QT", h.Le, today)
	if err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		lines, err := documents.BuildLines(ctx, tx, h, today, in.Lines, ev, nil)
		if err != nil {
			return idem.Outcome{}, err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO quotation (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, valid_until, owner_id, department_id, department_path, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, number, h.Le.ID, h.Cust.ID,
			h.Cust.Name, h.Cust.TaxID, h.Currency, in.ValidUntil, h.Owner, h.DeptID, h.DeptPath,
			search.Text(number, h.Cust.Name), search.Version); err != nil {
			return idem.Outcome{}, err
		}
		if err := documents.WriteLines(ctx, tx, "quotation", id, lines); err != nil {
			return idem.Outcome{}, err
		}
		q, err := documents.EmitQuotation(m, ctx, tx, id)
		if err != nil {
			return idem.Outcome{}, err
		}
		return documents.IDResult(id), core.Change(m, ctx, tx, actor, core.OpQuoteEdit, core.Quotations, id, q)
	})
	if err != nil {
		return err
	}
	q, err := documents.Quotation(m, ctx, documents.ResultID(out))
	if err != nil {
		return err
	}
	return c.JSON(200, q)
}

func GetQuotation(m *core.Base, c *httpx.Context) error {
	q, err := documents.Quotation(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := core.Check(c, core.OpQuoteRead, core.DocFacts(q.LegalEntityID, q.DepartmentPath, q.OwnerID)); err != nil {
		return err
	}
	return c.JSON(200, q)
}

func UpdateQuotation(m *core.Base, c *httpx.Context) error {
	var in documents.PatchIn
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.BusinessDate != nil {
		return core.Invalid("business_date", "a quotation has no business date")
	}
	if in.ValidUntil != nil && !core.DateForm.MatchString(*in.ValidUntil) {
		return core.Invalid("valid_until", "valid_until is YYYY-MM-DD")
	}
	cur, err := documents.Quotation(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, h, ev, err := documents.PatchHead(m, c, core.OpQuoteEdit, &in, cur.LegalEntityID, cur.Currency, cur.Customer.PartnerID,
		cur.OwnerID, cur.DepartmentID, cur.DepartmentPath)
	if err != nil {
		return err
	}
	var out core.Quotation
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		q, err := core.GetQuotation(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if q.State != "draft" {
			return core.ErrState.New("only a draft quotation changes; this one is " + q.State)
		}
		if in.Version != nil && *in.Version != q.Version {
			return core.ErrVersion.New(fmt.Sprintf("the quotation is at version %d", q.Version))
		}
		if in.Lines != nil {
			today, err := bizdate.Today(h.Le.Timezone)
			if err != nil {
				return err
			}
			lines, err := documents.BuildLines(ctx, tx, h, today, in.Lines, ev, q.Lines)
			if err != nil {
				return err
			}
			if err := documents.WriteLines(ctx, tx, "quotation", q.ID, lines); err != nil {
				return err
			}
		}
		validUntil := q.ValidUntil
		if in.ValidUntil != nil {
			validUntil = in.ValidUntil
		}
		if _, err := tx.Exec(ctx, `UPDATE quotation SET owner_id = $2, department_id = $3, department_path = $4,
			valid_until = $5::date, version = version + 1, updated_at = now() WHERE id = $1`,
			q.ID, h.Owner, h.DeptID, h.DeptPath, validUntil); err != nil {
			return err
		}
		if out, err = documents.EmitQuotation(m, ctx, tx, q.ID); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpQuoteEdit, core.Quotations, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

// QuotationAction sends or cancels a quotation; repeating the action on a quotation already in its state answers it
// as it is.
func QuotationAction(m *core.Base, action string) httpx.Handler {
	from := map[string][]string{"send": {"draft"}, "cancel": {"draft", "sent"}}[action]
	to := map[string]string{"send": "sent", "cancel": "cancelled"}[action]
	return func(c *httpx.Context) error {
		cur, err := documents.Quotation(m, c.Ctx(), c.Param("id"))
		if err != nil {
			return err
		}
		ctx, actor, err := core.Check(c, core.OpQuoteEdit, core.DocFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID))
		if err != nil {
			return err
		}
		var out core.Quotation
		err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
			q, err := core.GetQuotation(ctx, tx, cur.ID, true)
			if err != nil {
				return err
			}
			if q.State == to {
				out = q
				return nil
			}
			if !contains(from, q.State) {
				return core.ErrState.New("a " + q.State + " quotation cannot be " + to)
			}
			if action == "send" && len(q.Lines) == 0 { // a draft made from a quote request until a user adds lines (QR05)
				return core.Invalid("lines", "a quotation is sent with lines")
			}
			if _, err := tx.Exec(ctx, `UPDATE quotation SET state = $2, version = version + 1, updated_at = now()
				WHERE id = $1`, q.ID, to); err != nil {
				return err
			}
			if out, err = documents.EmitQuotation(m, ctx, tx, q.ID); err != nil {
				return err
			}
			return core.Change(m, ctx, tx, actor, core.OpQuoteEdit, core.Quotations, out.ID, out)
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

// AcceptQuotation turns a sent quotation into an order draft with the same lines and prices; accepting it again
// answers the quotation with its order.
func AcceptQuotation(m *core.Base, c *httpx.Context) error {
	cur, err := documents.Quotation(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	facts := core.DocFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID)
	ctx, actor, err := core.Check(c, core.OpQuoteEdit, facts)
	if err != nil {
		return err
	}
	if cur.State == "accepted" {
		return c.JSON(200, cur)
	}
	if _, _, err := core.Check(c, core.OpOrderEdit, facts); err != nil {
		return err
	}
	le, err := masterdata.LegalEntityOf(m, ctx, cur.LegalEntityID)
	if err != nil {
		return err
	}
	today, err := bizdate.Today(le.Timezone)
	if err != nil {
		return err
	}
	if cur.ValidUntil != nil && *cur.ValidUntil < today {
		return core.ErrState.New("the quotation was valid until " + *cur.ValidUntil)
	}
	number, err := documents.Number(m, ctx, "SO", le, today)
	if err != nil {
		return err
	}
	var out core.Quotation
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		q, err := core.GetQuotation(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		if q.State == "accepted" {
			out = q
			return nil
		}
		if q.State != "sent" {
			return core.ErrState.New("only a sent quotation is accepted; this one is " + q.State)
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
		o, err := core.EmitOrder(m, ctx, tx, orderID)
		if err != nil {
			return err
		}
		if err := core.Change(m, ctx, tx, actor, core.OpOrderEdit, core.Orders, o.ID, o); err != nil {
			return err
		}
		if out, err = documents.EmitQuotation(m, ctx, tx, q.ID); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpQuoteEdit, core.Quotations, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

var quotationList = list.Spec{Operation: core.OpQuoteRead, Alias: "q", IDColumn: "id", IDKind: list.UUID,
	Columns: documents.DocColumns(), Search: documents.NumberCustomer}

func ListQuotations(m *core.Base, c *httpx.Context) error {
	f, err := documents.Filters(c, documents.DocParams)
	if err != nil {
		return err
	}
	ctx, q, err := documents.Build(c, quotationList, f)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, "quotation", core.QuotationCols, core.ScanQuotation,
		func(r core.Quotation) ([]string, string) { return []string{r.CreatedAt}, r.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"quotations": out, "next_cursor": next})
}
