package ledger

import (
	"context"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/ids"
)

// The billable ledger of the order lines (protocol 05; this component owns it): per line and per dimension (quantity,
// amount) billable, claimed and billed, with claimed + billed <= billable (the table's CHECKs are the net), and the
// delivered quantity it grows from. These five functions are the only writers of those columns
// (TestOnlyTheLedgerWritesTheLedger); each runs in the caller's transaction under the order's lock and publishes the
// changed lines with LedgerFacts.

// Move is a quantity and an amount on one order line.
type Move struct {
	Line     string
	Quantity string
	Amount   string
}

// Claim adds a new claim's lines to claimed.
func Claim(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity string, lines []Move) error {
	ids := make([]string, len(lines))
	for i, l := range lines {
		ids[i] = l.Line
		if _, err := tx.Exec(ctx, `UPDATE order_line SET claimed_qty = claimed_qty + $2::numeric,
			claimed_amt = claimed_amt + $3::numeric WHERE id = $1`, l.Line, l.Quantity, l.Amount); err != nil {
			return err
		}
	}
	return LedgerFacts(m, ctx, tx, orderID, legalEntity, ids, "claim")
}

// GiveBack takes a pending or confirmed claim's lines (billing_claim_line) back out of claimed: released or expired.
func GiveBack(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity, claimID string, lines []string, cause string) error {
	if _, err := tx.Exec(ctx, `UPDATE order_line l SET claimed_qty = l.claimed_qty - cl.quantity,
		claimed_amt = l.claimed_amt - cl.amount FROM billing_claim_line cl WHERE cl.claim_id = $1 AND cl.order_line_id = l.id`,
		claimID); err != nil {
		return err
	}
	return LedgerFacts(m, ctx, tx, orderID, legalEntity, lines, cause)
}

// Bill formalizes a claim: each line's claim leaves claimed and what was billed of it enters billed.
func Bill(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity string, claimed, billed []Move) error {
	ids := make([]string, len(claimed))
	for i, c := range claimed {
		ids[i] = c.Line
		b := billed[i]
		if _, err := tx.Exec(ctx, `UPDATE order_line SET claimed_qty = claimed_qty - $2::numeric,
			claimed_amt = claimed_amt - $3::numeric, billed_qty = billed_qty + $4::numeric, billed_amt = billed_amt + $5::numeric
			WHERE id = $1`, c.Line, c.Quantity, c.Amount, b.Quantity, b.Amount); err != nil {
			return err
		}
	}
	return LedgerFacts(m, ctx, tx, orderID, legalEntity, ids, "formalize")
}

// Deliver posts a delivery: each line's delivered and billable quantity grow by the delivered quantity, and the
// billable amount by its share of the line amount rounded to scale, the last delivery making it the line amount
// (docs/rules/pricing.md, "可出账金额").
func Deliver(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity, deliveryID string, scale int32) error {
	rows, err := tx.Query(ctx, `UPDATE order_line l SET delivered = l.delivered + dl.quantity,
		billable_qty = l.billable_qty + dl.quantity,
		billable_amt = CASE WHEN l.delivered + dl.quantity = l.quantity THEN l.net
			ELSE l.billable_amt + round(l.net * dl.quantity / l.quantity, $2) END
		FROM delivery_line dl WHERE dl.delivery_id = $1 AND dl.order_line_id = l.id RETURNING l.id::text`, deliveryID, scale)
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
	if err := rows.Err(); err != nil {
		return err
	}
	return LedgerFacts(m, ctx, tx, orderID, legalEntity, changed, "delivery")
}

// ServiceConfirmed makes the service lines of a confirmed order billable whole (they are not delivered).
func ServiceConfirmed(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity string) error {
	rows, err := tx.Query(ctx, `UPDATE order_line SET billable_qty = quantity, billable_amt = net
		WHERE order_id = $1 AND kind = 'service' RETURNING id::text`, orderID)
	if err != nil {
		return err
	}
	var service []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		service = append(service, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return LedgerFacts(m, ctx, tx, orderID, legalEntity, service, "service_confirmed")
}

// LedgerFacts publishes erp.billable.changed.v1 for each changed line, with its ledger after the change.
func LedgerFacts(m *core.Base, ctx context.Context, tx db.Tx, orderID, legalEntity string, lines []string, cause string) error {
	if len(lines) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text, trim_scale(billable_qty)::text, trim_scale(billable_amt)::text,
		trim_scale(claimed_qty)::text, trim_scale(claimed_amt)::text, trim_scale(billed_qty)::text, trim_scale(billed_amt)::text
		FROM order_line WHERE id = ANY($1::uuid[]) ORDER BY line_no`, lines)
	if err != nil {
		return err
	}
	type fact struct {
		FactID      string       `json:"fact_id"`
		OrderID     string       `json:"order_id"`
		OrderLineID string       `json:"order_line_id"`
		Cause       string       `json:"cause"`
		Billable    core.Measure `json:"billable"`
		Claimed     core.Measure `json:"claimed"`
		Billed      core.Measure `json:"billed"`
		OccurredAt  string       `json:"occurred_at"`
	}
	var facts []fact
	for rows.Next() {
		f := fact{FactID: ids.New().String(), OrderID: orderID, Cause: cause, OccurredAt: canon.Time(time.Now())}
		if err := rows.Scan(&f.OrderLineID, &f.Billable.Quantity, &f.Billable.Amount, &f.Claimed.Quantity,
			&f.Claimed.Amount, &f.Billed.Quantity, &f.Billed.Amount); err != nil {
			rows.Close()
			return err
		}
		facts = append(facts, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, f := range facts {
		if err := core.Emit(m, ctx, tx, core.BillableChanged, f.FactID, 1, legalEntity, f); err != nil {
			return err
		}
	}
	return nil
}
