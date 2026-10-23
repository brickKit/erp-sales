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

// Order drafts: creating, reading and changing them; confirming is confirmation.go.

type orderIn struct {
	LegalEntityID string   `json:"legal_entity_id"`
	CustomerID    string   `json:"customer_id"`
	Currency      string   `json:"currency"`
	OwnerID       *string  `json:"owner_id"`
	DepartmentID  *string  `json:"department_id"`
	BusinessDate  *string  `json:"business_date"`
	Lines         []lineIn `json:"lines"`
}

func (m *Module) createOrder(c *httpx.Context) error {
	var in orderIn
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.BusinessDate != nil && !dateForm.MatchString(*in.BusinessDate) {
		return invalid("business_date", "business_date is YYYY-MM-DD")
	}
	ctx, actor, h, ev, err := m.prepare(c, OpOrderEdit, in.LegalEntityID, in.CustomerID, in.Currency, in.OwnerID,
		in.DepartmentID, in.Lines)
	if err != nil {
		return err
	}
	date := ""
	if in.BusinessDate != nil {
		date = *in.BusinessDate
	} else if date, err = bizdate.Today(h.le.Timezone); err != nil {
		return err
	}
	number, err := m.number(ctx, "SO", h.le, date)
	if err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		lines, err := buildLines(ctx, tx, h, date, in.Lines, ev, nil)
		if err != nil {
			return idem.Outcome{}, err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO sales_order (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, business_date, owner_id, department_id, department_path, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, number, h.le.ID, h.cust.ID,
			h.cust.Name, h.cust.TaxID, h.currency, date, h.owner, h.deptID, h.deptPath, search.Text(number, h.cust.Name),
			search.Version); err != nil {
			return idem.Outcome{}, err
		}
		if err := writeLines(ctx, tx, "sales_order", id, lines); err != nil {
			return idem.Outcome{}, err
		}
		o, err := m.emitOrder(ctx, tx, id)
		if err != nil {
			return idem.Outcome{}, err
		}
		return idResult(id), m.change(ctx, tx, actor, OpOrderEdit, Orders, id, o)
	})
	if err != nil {
		return err
	}
	o, err := m.order(ctx, resultID(out))
	if err != nil {
		return err
	}
	return c.JSON(200, o)
}

func (m *Module) getOrder(c *httpx.Context) error {
	o, err := m.order(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpOrderRead, docFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID)); err != nil {
		return err
	}
	return c.JSON(200, o)
}

// updateOrder changes a draft; a confirming order refuses every change (RS09).
func (m *Module) updateOrder(c *httpx.Context) error {
	var in patchIn
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.ValidUntil != nil {
		return invalid("valid_until", "an order has no valid_until")
	}
	if in.BusinessDate != nil && !dateForm.MatchString(*in.BusinessDate) {
		return invalid("business_date", "business_date is YYYY-MM-DD")
	}
	cur, err := m.order(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, h, ev, err := m.patchHead(c, OpOrderEdit, &in, cur.LegalEntityID, cur.Currency, cur.Customer.PartnerID,
		cur.OwnerID, cur.DepartmentID, cur.DepartmentPath)
	if err != nil {
		return err
	}
	var out Order
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		o, err := getOrder(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		switch {
		case o.State == "confirming":
			return errState.New("the order is being confirmed; it changes after the confirmation ends")
		case o.State != "draft":
			return errState.New("only a draft order changes; this one is " + o.State + " (revisions come later)")
		case in.Version != nil && *in.Version != o.Version:
			return errVersion.New(fmt.Sprintf("the order is at version %d", o.Version))
		}
		date := o.BusinessDate
		if in.BusinessDate != nil {
			date = *in.BusinessDate
		}
		if in.Lines != nil {
			prev := make([]QuoteLine, len(o.Lines))
			for i, l := range o.Lines {
				prev[i] = l.QuoteLine
			}
			lines, err := buildLines(ctx, tx, h, date, in.Lines, ev, prev)
			if err != nil {
				return err
			}
			if err := writeLines(ctx, tx, "sales_order", o.ID, lines); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sales_order SET owner_id = $2, department_id = $3, department_path = $4,
			business_date = $5::date, version = version + 1, updated_at = now() WHERE id = $1`,
			o.ID, h.owner, h.deptID, h.deptPath, date); err != nil {
			return err
		}
		if out, err = m.emitOrder(ctx, tx, o.ID); err != nil {
			return err
		}
		return m.change(ctx, tx, actor, OpOrderEdit, Orders, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}
