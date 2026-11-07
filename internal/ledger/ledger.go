package ledger

import (
	"context"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/ids"
)

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
