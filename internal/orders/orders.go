package orders

import (
	"context"
	"fmt"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	documents "github.com/brickKit/erp-sales/v3/internal/documents"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/list"
	"github.com/brickKit/be-sdk-go/v3/search"
)

// Order drafts: creating, reading and changing them; confirming is internal/confirmations.

type orderIn struct {
	LegalEntityID string             `json:"legal_entity_id"`
	CustomerID    string             `json:"customer_id"`
	Currency      string             `json:"currency"`
	OwnerID       *string            `json:"owner_id"`
	DepartmentID  *string            `json:"department_id"`
	BusinessDate  *string            `json:"business_date"`
	Lines         []documents.LineIn `json:"lines"`
}

func CreateOrder(m *core.Base, c *httpx.Context) error {
	var in orderIn
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.BusinessDate != nil && !core.DateForm.MatchString(*in.BusinessDate) {
		return core.Invalid("business_date", "business_date is YYYY-MM-DD")
	}
	ctx, actor, h, ev, err := documents.Prepare(m, c, core.OpOrderEdit, in.LegalEntityID, in.CustomerID, in.Currency, in.OwnerID,
		in.DepartmentID, in.Lines)
	if err != nil {
		return err
	}
	date := ""
	if in.BusinessDate != nil {
		date = *in.BusinessDate
	} else if date, err = bizdate.Today(h.Le.Timezone); err != nil {
		return err
	}
	number, err := documents.Number(m, ctx, "SO", h.Le, date)
	if err != nil {
		return err
	}
	out, _, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		lines, err := documents.BuildLines(ctx, tx, h, date, in.Lines, ev, nil)
		if err != nil {
			return idem.Outcome{}, err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO sales_order (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, business_date, owner_id, department_id, department_path, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11, $12, $13)`, id, number, h.Le.ID, h.Cust.ID,
			h.Cust.Name, h.Cust.TaxID, h.Currency, date, h.Owner, h.DeptID, h.DeptPath, search.Text(number, h.Cust.Name),
			search.Version); err != nil {
			return idem.Outcome{}, err
		}
		if err := documents.WriteLines(ctx, tx, "sales_order", id, lines); err != nil {
			return idem.Outcome{}, err
		}
		o, err := core.EmitOrder(m, ctx, tx, id)
		if err != nil {
			return idem.Outcome{}, err
		}
		return documents.IDResult(id), core.Change(m, ctx, tx, actor, core.OpOrderEdit, core.Orders, id, o)
	})
	if err != nil {
		return err
	}
	o, err := documents.Order(m, ctx, documents.ResultID(out))
	if err != nil {
		return err
	}
	return c.JSON(200, o)
}

func GetOrder(m *core.Base, c *httpx.Context) error {
	// the archive first (assembly protocol 11, be-protocol P15.22): a release or a destruction deletes an order's online
	// rows in batches, so the online tables alone can show part of an order that has left them; a destroyed order
	// answers NOT_FOUND from the decision on
	o, found, err := archived(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if !found {
		if o, err = documents.Order(m, c.Ctx(), c.Param("id")); err != nil {
			return err
		}
	}
	if _, _, err := core.Check(c, core.OpOrderRead, core.DocFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID)); err != nil {
		return err
	}
	return c.JSON(200, o)
}

// UpdateOrder changes a draft; a confirming order refuses every change (RS09).
func UpdateOrder(m *core.Base, c *httpx.Context) error {
	var in documents.PatchIn
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if in.ValidUntil != nil {
		return core.Invalid("valid_until", "an order has no valid_until")
	}
	if in.BusinessDate != nil && !core.DateForm.MatchString(*in.BusinessDate) {
		return core.Invalid("business_date", "business_date is YYYY-MM-DD")
	}
	cur, err := documents.Order(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, h, ev, err := documents.PatchHead(m, c, core.OpOrderEdit, &in, cur.LegalEntityID, cur.Currency, cur.Customer.PartnerID,
		cur.OwnerID, cur.DepartmentID, cur.DepartmentPath)
	if err != nil {
		return err
	}
	var out core.Order
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		o, err := core.GetOrder(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		ph, err := PhaseOf(ctx, tx, o)
		if err != nil {
			return err
		}
		if d := Decide(ph, Edit); d.Effect == Refuse {
			return Refused(d, o)
		}
		if in.Version != nil && *in.Version != o.Version {
			return core.ErrVersion.New(fmt.Sprintf("the order is at version %d", o.Version))
		}
		date := o.BusinessDate
		if in.BusinessDate != nil {
			date = *in.BusinessDate
		}
		if in.Lines != nil {
			prev := make([]core.QuoteLine, len(o.Lines))
			for i, l := range o.Lines {
				prev[i] = l.QuoteLine
			}
			lines, err := documents.BuildLines(ctx, tx, h, date, in.Lines, ev, prev)
			if err != nil {
				return err
			}
			if err := documents.WriteLines(ctx, tx, "sales_order", o.ID, lines); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sales_order SET owner_id = $2, department_id = $3, department_path = $4,
			business_date = $5::date, version = version + 1, updated_at = now() WHERE id = $1`,
			o.ID, h.Owner, h.DeptID, h.DeptPath, date); err != nil {
			return err
		}
		if out, err = core.EmitOrder(m, ctx, tx, o.ID); err != nil {
			return err
		}
		return core.Change(m, ctx, tx, actor, core.OpOrderEdit, core.Orders, out.ID, out)
	})
	if err != nil {
		return err
	}
	return c.JSON(200, out)
}

var orderList = list.Spec{Operation: core.OpOrderRead, Alias: "o", IDColumn: "id", IDKind: list.UUID, Search: documents.NumberCustomer,
	Columns: documents.DocColumns(
		list.Column{Field: "to_deliver", Column: "to_deliver", Kind: list.Boolean, Filterable: true},
		list.Column{Field: "to_bill", Column: "to_bill", Kind: list.Boolean, Filterable: true})}

// orderFrom adds the two progress filters: goods left to deliver on a confirmed order, billable left on any line. The
// list's default range (assembly protocol 11, TR10): every order not ended, and those created in the last N days;
// history=true reads them all, archived ones included.
const orderFromAll = `(SELECT s.*,
	s.state = 'confirmed' AND EXISTS (SELECT 1 FROM order_line l WHERE l.order_id = s.id AND l.kind = 'goods'
		AND l.delivered < l.quantity) AS to_deliver,
	EXISTS (SELECT 1 FROM order_line l WHERE l.order_id = s.id AND (l.claimed_qty + l.billed_qty < l.billable_qty
		OR l.claimed_amt + l.billed_amt < l.billable_amt)) AS to_bill
	FROM sales_order s %s)`

func orderFrom(m *core.Base) string {
	days := 90
	if m.RT.Tiering != nil {
		days = int(m.RT.Tiering.N.Hours() / 24)
	}
	return fmt.Sprintf(orderFromAll, fmt.Sprintf("WHERE s.state NOT IN ('completed', 'cancelled') OR s.created_at > "+
		"now() - make_interval(days => %d)", days))
}

func ListOrders(m *core.Base, c *httpx.Context) error {
	if c.Query("history") == "true" {
		return history(m, c)
	}
	params := map[string]string{"to_deliver": "to_deliver", "to_bill": "to_bill"}
	for k, v := range documents.DocParams {
		params[k] = v
	}
	f, err := documents.Filters(c, params)
	if err != nil {
		return err
	}
	ctx, q, err := documents.Build(c, orderList, f)
	if err != nil {
		return err
	}
	out, next, err := list.Page(ctx, m.RT.DB, q, orderFrom(m), core.OrderCols, core.ScanOrder,
		func(r core.Order) ([]string, string) { return []string{r.CreatedAt}, r.ID })
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"orders": out, "next_cursor": next})
}

func UnsupportedOnOrder(m *core.Base, what string) httpx.Handler {
	return func(c *httpx.Context) error {
		o, err := documents.Order(m, c.Ctx(), c.Param("id"))
		if err != nil {
			return err
		}
		if _, _, err := core.Check(c, c.Operation(), core.DocFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID)); err != nil {
			return err
		}
		return errs.New(errs.ReasonCapabilityUnsupported, what+" is not in this version of erp/sales")
	}
}
