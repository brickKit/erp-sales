package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/work"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
)

// Deliveries (docs/flows/delivery-and-billing.md): T1 records the delivery issuing, the inventory issues it and
// consumes the order's branch (the delivery ID is the consumption ID), T2 posts it and grows the billable ledger.

const jobIssue = "issue-delivery"

type deliveryLineIn struct {
	OrderLineID string   `json:"order_line_id"`
	Quantity    string   `json:"quantity"`
	Batch       *string  `json:"batch"`
	Serials     []string `json:"serials"`
}

type deliveryIn struct {
	OrderID      string           `json:"order_id"`
	BusinessDate *string          `json:"business_date"`
	Lines        []deliveryLineIn `json:"lines"`
}

func (m *Module) delivery(ctx context.Context, id string) (Delivery, error) {
	var d Delivery
	if !uuidForm.MatchString(id) {
		return d, notFound("delivery")
	}
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		d, err = getDelivery(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) {
		return d, notFound("delivery")
	}
	return d, err
}

func (m *Module) emitDelivery(ctx context.Context, tx db.Tx, id string) (Delivery, error) {
	d, err := getDelivery(ctx, tx, id, false)
	if err != nil {
		return d, err
	}
	return d, m.emit(ctx, tx, DeliveryChanged, d.ID, d.Version, d.LegalEntityID, d)
}

func (m *Module) createDelivery(c *httpx.Context) error {
	var in deliveryIn
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	if err := c.BindJSON(&in); err != nil {
		return err
	}
	if !uuidForm.MatchString(in.OrderID) {
		return invalid("order_id", "order_id is an order's ID")
	}
	if in.BusinessDate != nil && !dateForm.MatchString(*in.BusinessDate) {
		return invalid("business_date", "business_date is YYYY-MM-DD")
	}
	if len(in.Lines) == 0 || len(in.Lines) > MaxLines {
		return invalid("lines", fmt.Sprintf("a delivery has 1-%d lines", MaxLines))
	}
	o, err := m.order(c.Ctx(), in.OrderID)
	if err != nil {
		return err
	}
	ctx, actor, err := check(c, OpDeliveryCreate, docFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID))
	if err != nil {
		return err
	}
	lines := map[string]OrderLine{}
	for _, l := range o.Lines {
		lines[l.ID] = l
	}
	base := make([]string, len(in.Lines))
	seen := map[string]bool{}
	for i := range in.Lines {
		l := &in.Lines[i]
		field := fmt.Sprintf("lines[%d]", i)
		ol, ok := lines[l.OrderLineID]
		switch {
		case !ok:
			return invalid(field+".order_line_id", "no such line on the order")
		case ol.Kind != "goods":
			return invalid(field+".order_line_id", "a service line is not delivered")
		case seen[l.OrderLineID]:
			return invalid(field+".order_line_id", "one entry per order line")
		}
		seen[l.OrderLineID] = true
		if l.Quantity, err = decimal(field+".quantity", l.Quantity, QuantityScale, true); err != nil {
			return err
		}
		if base[i], _, err = m.convert(ctx, ol.Product.ID, l.Quantity, ol.Unit, ol.Product.BaseUnit); err != nil {
			return err
		}
	}
	le, err := m.legalEntityOf(ctx, o.LegalEntityID)
	if err != nil {
		return err
	}
	date := ""
	if in.BusinessDate != nil {
		date = *in.BusinessDate
	} else if date, err = bizdate.Today(le.Timezone); err != nil {
		return err
	}
	number, err := m.number(ctx, "DN", le, date)
	if err != nil {
		return err
	}
	out, replayed, err := idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		cur, err := getOrder(ctx, tx, o.ID, true)
		if err != nil {
			return idem.Outcome{}, err
		}
		if cur.State != "confirmed" {
			return idem.Outcome{}, errState.New("only a confirmed order is delivered; this one is " + cur.State)
		}
		if open, err := openCancel(ctx, tx, o.ID); err != nil || open {
			if err == nil {
				err = errState.New("the order is being cancelled")
			}
			return idem.Outcome{}, err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO delivery (id, number, order_id, legal_entity_id, owner_id, department_id,
			department_path, business_date) SELECT $2, $3, id, legal_entity_id, owner_id, department_id, department_path,
			$4::date FROM sales_order WHERE id = $1`, o.ID, id, number, date); err != nil {
			return idem.Outcome{}, err
		}
		for i, l := range in.Lines {
			// at most what is neither delivered nor being delivered
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT l.quantity - l.delivered - coalesce((SELECT sum(dl.quantity)
				FROM delivery_line dl JOIN delivery d ON d.id = dl.delivery_id
				WHERE dl.order_line_id = l.id AND d.state = 'issuing'), 0) >= $2::numeric
				FROM order_line l WHERE l.id = $1`, l.OrderLineID, l.Quantity).Scan(&ok); err != nil {
				return idem.Outcome{}, err
			}
			if !ok {
				return idem.Outcome{}, errState.New(fmt.Sprintf("line %d delivers more than is left to deliver on order line %s",
					i+1, l.OrderLineID))
			}
			ol := lines[l.OrderLineID]
			serials := l.Serials
			if serials == nil {
				serials = []string{}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO delivery_line (delivery_id, order_line_id, product_id, warehouse_id,
				quantity, base_quantity, batch, serials) VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8)`,
				id, l.OrderLineID, ol.Product.ID, ol.WarehouseID, l.Quantity, base[i], l.Batch, serials); err != nil {
				return idem.Outcome{}, err
			}
		}
		d, err := m.emitDelivery(ctx, tx, id)
		if err != nil {
			return idem.Outcome{}, err
		}
		if err := m.change(ctx, tx, actor, OpDeliveryCreate, Deliveries, id, d); err != nil {
			return idem.Outcome{}, err
		}
		if _, err := m.rt.Work.Enqueue(ctx, tx, jobIssue, issueArgs{ID: id}); err != nil {
			return idem.Outcome{}, err
		}
		return idResult(id), nil
	})
	if err != nil {
		return err
	}
	id := resultID(out)
	if !replayed {
		sync, cancel := context.WithTimeout(ctx, SyncBudget)
		_ = m.issue(sync, id) // what is left is the work item's
		cancel()
	}
	d, err := m.delivery(ctx, id)
	if err != nil {
		return err
	}
	return c.JSON(200, d)
}

func (m *Module) getDelivery(c *httpx.Context) error {
	d, err := m.delivery(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpDeliveryRead, docFacts(d.LegalEntityID, d.DepartmentPath, d.OwnerID)); err != nil {
		return err
	}
	return c.JSON(200, d)
}

type issueArgs struct {
	ID string `json:"delivery_id"`
}

// issue asks the inventory to issue an issuing delivery and records its answer: posted or failed. An unknown answer
// (errUnknown) leaves the delivery issuing for the work item, which sends the same consumption ID again.
func (m *Module) issue(ctx context.Context, id string) error {
	d, err := m.delivery(ctx, id)
	if err != nil || d.State != "issuing" {
		return err
	}
	var branchID, currency string
	var revision int64
	err = m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT b.branch_id::text, o.currency, o.revision FROM sales_order o
			JOIN confirmation_branch b ON b.confirmation_id = o.confirmation_id AND b.component = $2
			WHERE o.id = $1`, d.OrderID, Inventory).Scan(&branchID, &currency, &revision)
	})
	if err != nil {
		return err
	}
	scale, err := m.currencyScale(ctx, currency)
	if err != nil {
		return err
	}
	conn, err := m.conn(Inventory)
	if err != nil {
		return err
	}
	req := &inventoryv3.IssueRequest{ConsumptionId: d.ID, BranchId: branchID, BusinessDate: d.BusinessDate,
		Reference: &inventoryv3.Reference{Kind: "sales_order", Id: d.OrderID, Revision: revision}}
	for _, l := range d.Lines {
		req.Lines = append(req.Lines, &inventoryv3.IssueLine{ProductId: l.ProductID, WarehouseId: l.WarehouseID,
			Quantity: l.BaseQuantity, Batch: deref(l.Batch), Serials: l.Serials, SourceLineId: l.OrderLineID})
	}
	res, err := inventoryv3.NewStockServiceClient(conn).Issue(ctx, req, calls.Idempotent())
	if err != nil {
		return fmt.Errorf("%w: %v", errUnknown, err)
	}
	if res.GetIssued() {
		return m.post(ctx, id, scale)
	}
	var why []string
	for _, p := range res.GetProblems() {
		why = append(why, fmt.Sprintf("line %d: %s %s", p.GetLine()+1, p.GetReason(), p.GetDetail()))
	}
	return m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE delivery SET state = 'failed', reason = $2, version = version + 1,
			updated_at = now() WHERE id = $1 AND state = 'issuing'`, id, strings.TrimSpace(strings.Join(why, "; ")))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = m.emitDelivery(ctx, tx, id)
		return err
	})
}

// post is T2: the delivery posted, the lines' delivered quantities and billable ledgers grow (docs/rules/pricing.md:
// the last delivery of a line makes its billable amount exactly the line amount).
func (m *Module) post(ctx context.Context, id string, scale int32) error {
	return m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var orderID string
		if err := tx.QueryRow(ctx, `SELECT order_id::text FROM delivery WHERE id = $1`, id).Scan(&orderID); err != nil {
			return err
		}
		if _, err := getOrder(ctx, tx, orderID, true); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE delivery SET state = 'posted', reason = NULL, version = version + 1,
			updated_at = now() WHERE id = $1 AND state = 'issuing'`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE order_line l SET delivered = l.delivered + dl.quantity,
			billable_qty = l.billable_qty + dl.quantity,
			billable_amt = CASE WHEN l.delivered + dl.quantity = l.quantity THEN l.net
				ELSE l.billable_amt + round(l.net * dl.quantity / l.quantity, $2) END
			FROM delivery_line dl WHERE dl.delivery_id = $1 AND dl.order_line_id = l.id RETURNING l.id::text`, id, scale)
		if err != nil {
			return err
		}
		var changed []string
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				rows.Close()
				return err
			}
			changed = append(changed, l)
		}
		rows.Close()
		d, err := m.emitDelivery(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := m.emit(ctx, tx, DeliveryPosted, d.ID, 1, d.LegalEntityID, map[string]any{"fact_id": d.ID,
			"order_id": d.OrderID, "legal_entity_id": d.LegalEntityID, "lines": d.Lines,
			"occurred_at": canon.Time(time.Now())}); err != nil {
			return err
		}
		if err := m.ledgerFacts(ctx, tx, d.OrderID, d.LegalEntityID, changed, "delivery"); err != nil {
			return err
		}
		_, err = m.emitOrder(ctx, tx, d.OrderID)
		return err
	})
}

// issueJob is the recovery of an issuing delivery (protocol 01, idempotent_external by the consumption ID).
func (m *Module) issueJob(ctx context.Context, r *work.Run) error {
	var a issueArgs
	if err := json.Unmarshal(r.Args, &a); err != nil {
		return work.Permanent(err)
	}
	var young bool
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT state = 'issuing' AND created_at > now() - make_interval(secs => $2)
			FROM delivery WHERE id = $1`, a.ID, SyncBudget.Seconds()).Scan(&young)
	})
	if err != nil {
		return err
	}
	if young {
		return work.Wait(errors.New("the request's synchronous path still has the delivery"))
	}
	return m.issue(ctx, a.ID)
}
