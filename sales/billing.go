package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/work"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
	"github.com/jackc/pgx/v5"
)

// The billing source (protocol 05's claim pattern, the billing use; docs/flows/delivery-and-billing.md): erp/billing
// claims the billable ledger of order lines. Every command locks the order row first, so the claims of one order are
// decided one at a time and per line and dimension claimed + billed <= billable (the table's CHECKs are the net).

// ledgerFacts publishes erp.billable.changed.v1 for each changed line, with its ledger after the change.
func (m *Module) ledgerFacts(ctx context.Context, tx db.Tx, orderID, legalEntity string, lines []string, cause string) error {
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
		FactID      string  `json:"fact_id"`
		OrderID     string  `json:"order_id"`
		OrderLineID string  `json:"order_line_id"`
		Cause       string  `json:"cause"`
		Billable    Measure `json:"billable"`
		Claimed     Measure `json:"claimed"`
		Billed      Measure `json:"billed"`
		OccurredAt  string  `json:"occurred_at"`
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
		if err := m.emit(ctx, tx, BillableChanged, f.FactID, 1, legalEntity, f); err != nil {
			return err
		}
	}
	return nil
}

// claimRow is a claim with its lines, as stored.
type claimRow struct {
	ID, OrderID, State string
	ExpiresAt          *time.Time
	Lines              []*salesv3.ClaimLine
	Billed             []*salesv3.ClaimLine
}

func getClaim(ctx context.Context, tx db.Tx, id string, lock bool) (claimRow, error) {
	q := `SELECT id::text, order_id::text, state, expires_at FROM billing_claim WHERE id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	var c claimRow
	if err := tx.QueryRow(ctx, q, id).Scan(&c.ID, &c.OrderID, &c.State, &c.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, errNone
		}
		return c, err
	}
	rows, err := tx.Query(ctx, `SELECT order_line_id::text, trim_scale(quantity)::text, trim_scale(amount)::text,
		trim_scale(billed_qty)::text, trim_scale(billed_amt)::text FROM billing_claim_line WHERE claim_id = $1
		ORDER BY order_line_id`, id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		var mq, ma string
		var bq, ba *string
		if err := rows.Scan(&line, &mq, &ma, &bq, &ba); err != nil {
			return c, err
		}
		c.Lines = append(c.Lines, &salesv3.ClaimLine{OrderLineId: line, Measure: &salesv3.Measure{Quantity: mq, Amount: ma}})
		if bq != nil {
			c.Billed = append(c.Billed, &salesv3.ClaimLine{OrderLineId: line, Measure: &salesv3.Measure{Quantity: *bq, Amount: *ba}})
		}
	}
	return c, rows.Err()
}

func (c claimRow) proto() *salesv3.Claim {
	out := &salesv3.Claim{ClaimId: c.ID, OrderId: c.OrderID, Lines: c.Lines, Billed: c.Billed,
		State: salesv3.ClaimState(salesv3.ClaimState_value["CLAIM_STATE_"+strings.ToUpper(c.State)])}
	if c.State == "pending" && c.ExpiresAt != nil {
		out.ExpiresAt = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

// claimLines checks and canonicalizes the lines of a command; one entry per order line.
func claimLines(lines []*salesv3.ClaimLine, field string) ([]*salesv3.ClaimLine, error) {
	seen := map[string]bool{}
	out := make([]*salesv3.ClaimLine, 0, len(lines))
	for i, l := range lines {
		f := fmt.Sprintf("%s[%d]", field, i)
		if !uuidForm.MatchString(l.GetOrderLineId()) || seen[l.GetOrderLineId()] {
			return nil, invalid(f+".order_line_id", "an order line's ID, once")
		}
		seen[l.GetOrderLineId()] = true
		q, err := decimal(f+".measure.quantity", l.GetMeasure().GetQuantity(), QuantityScale, false)
		if err != nil {
			return nil, err
		}
		a, err := decimal(f+".measure.amount", l.GetMeasure().GetAmount(), 4, false)
		if err != nil {
			return nil, err
		}
		out = append(out, &salesv3.ClaimLine{OrderLineId: l.GetOrderLineId(), Measure: &salesv3.Measure{Quantity: q, Amount: a}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderLineId < out[j].OrderLineId })
	return out, nil
}

func sameLines(a, b []*salesv3.ClaimLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetOrderLineId() != b[i].GetOrderLineId() || a[i].GetMeasure().GetQuantity() != b[i].GetMeasure().GetQuantity() ||
			a[i].GetMeasure().GetAmount() != b[i].GetMeasure().GetAmount() {
			return false
		}
	}
	return true
}

func caller(ctx context.Context) string {
	c, _ := grpcx.Caller(ctx)
	return c
}

// claimTx runs fn on a claim under its order's lock, then the claim's; NOT_FOUND when there is no claim.
func (m *Module) claimTx(ctx context.Context, id string, fn func(ctx context.Context, tx db.Tx, c claimRow) error) error {
	if !uuidForm.MatchString(id) {
		return invalid("claim_id", "claim_id is a UUID")
	}
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var orderID string
		if err := tx.QueryRow(ctx, `SELECT order_id::text FROM billing_claim WHERE id = $1`, id).Scan(&orderID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNone
			}
			return err
		}
		if _, err := getOrder(ctx, tx, orderID, true); err != nil {
			return err
		}
		c, err := getClaim(ctx, tx, id, true)
		if err != nil {
			return err
		}
		return fn(ctx, tx, c)
	})
	if errors.Is(err, errNone) {
		return notFound("claim")
	}
	return err
}

// giveBack returns a pending or confirmed claim's quantities to the ledger and ends it in state.
func (m *Module) giveBack(ctx context.Context, tx db.Tx, c claimRow, state, cause string) error {
	var legalEntity string
	if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM sales_order WHERE id = $1`, c.OrderID).Scan(&legalEntity); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE order_line l SET claimed_qty = l.claimed_qty - cl.quantity,
		claimed_amt = l.claimed_amt - cl.amount FROM billing_claim_line cl WHERE cl.claim_id = $1 AND cl.order_line_id = l.id`,
		c.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_claim SET state = $2, expires_at = NULL, updated_at = now() WHERE id = $1`,
		c.ID, state); err != nil {
		return err
	}
	lines := make([]string, len(c.Lines))
	for i, l := range c.Lines {
		lines[i] = l.OrderLineId
	}
	return m.ledgerFacts(ctx, tx, c.OrderID, legalEntity, lines, cause)
}

// Ledger answers an order's billable ledger.
func (m *Module) Ledger(ctx context.Context, in *salesv3.LedgerRequest) (*salesv3.LedgerResponse, error) {
	o, err := m.order(ctx, in.GetOrderId())
	if err != nil {
		return nil, err
	}
	out := &salesv3.LedgerResponse{OrderId: o.ID, OrderState: o.State, OrderVersion: o.Version}
	for _, l := range o.Lines {
		out.Lines = append(out.Lines, &salesv3.LedgerLine{OrderLineId: l.ID,
			Billable: &salesv3.Measure{Quantity: l.Billable.Quantity, Amount: l.Billable.Amount},
			Claimed:  &salesv3.Measure{Quantity: l.Claimed.Quantity, Amount: l.Claimed.Amount},
			Billed:   &salesv3.Measure{Quantity: l.Billed.Quantity, Amount: l.Billed.Amount}})
	}
	return out, nil
}

// Claim takes the lines all or none (BL01); the same claim ID and content answer the first claim (BL02).
func (m *Module) Claim(ctx context.Context, in *salesv3.ClaimRequest) (*salesv3.ClaimResponse, error) {
	if !uuidForm.MatchString(in.GetClaimId()) || !uuidForm.MatchString(in.GetOrderId()) {
		return nil, invalid("claim_id", "claim_id and order_id are UUIDs")
	}
	if len(in.GetLines()) == 0 || len(in.GetLines()) > MaxLines {
		return nil, invalid("lines", fmt.Sprintf("a claim has 1-%d lines", MaxLines))
	}
	lines, err := claimLines(in.GetLines(), "lines")
	if err != nil {
		return nil, err
	}
	var out *salesv3.ClaimResponse
	err = m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		o, err := getOrder(ctx, tx, in.GetOrderId(), true)
		if errors.Is(err, errNone) {
			out = &salesv3.ClaimResponse{Problems: []*salesv3.ClaimProblem{{Reason: "order_not_billable"}}}
			return nil
		}
		if err != nil {
			return err
		}
		if c, err := getClaim(ctx, tx, in.GetClaimId(), true); err == nil {
			if c.OrderID != o.ID || !sameLines(c.Lines, lines) {
				return errContent.New("claim " + c.ID + " was made with other content")
			}
			out = &salesv3.ClaimResponse{Claimed: true, Claim: c.proto()}
			return nil
		} else if !errors.Is(err, errNone) {
			return err
		}
		open, err := openCancel(ctx, tx, o.ID)
		if err != nil {
			return err
		}
		if o.State != "confirmed" || open {
			out = &salesv3.ClaimResponse{Problems: []*salesv3.ClaimProblem{{Reason: "order_not_billable"}}}
			return nil
		}
		byID := map[string]OrderLine{}
		for _, l := range o.Lines {
			byID[l.ID] = l
		}
		var problems []*salesv3.ClaimProblem
		for _, l := range lines {
			ol, ok := byID[l.OrderLineId]
			if !ok {
				problems = append(problems, &salesv3.ClaimProblem{OrderLineId: l.OrderLineId, Reason: "line_not_found"})
				continue
			}
			var fits bool
			var rq, ra string
			if err := tx.QueryRow(ctx, `SELECT claimed_qty + billed_qty + $2::numeric <= billable_qty
				AND claimed_amt + billed_amt + $3::numeric <= billable_amt,
				trim_scale(billable_qty - claimed_qty - billed_qty)::text, trim_scale(billable_amt - claimed_amt - billed_amt)::text
				FROM order_line WHERE id = $1`, ol.ID, l.Measure.Quantity, l.Measure.Amount).Scan(&fits, &rq, &ra); err != nil {
				return err
			}
			if !fits {
				problems = append(problems, &salesv3.ClaimProblem{OrderLineId: ol.ID, Reason: "exceeds_billable",
					Remaining: &salesv3.Measure{Quantity: rq, Amount: ra}})
			}
		}
		if len(problems) > 0 {
			out = &salesv3.ClaimResponse{Problems: problems}
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_claim (id, order_id, state, caller, expires_at)
			VALUES ($1, $2, 'pending', $3, now() + make_interval(secs => $4))`, in.GetClaimId(), o.ID, caller(ctx),
			ClaimPeriod.Seconds()); err != nil {
			return err
		}
		ids := make([]string, len(lines))
		for i, l := range lines {
			ids[i] = l.OrderLineId
			if _, err := tx.Exec(ctx, `INSERT INTO billing_claim_line (claim_id, order_line_id, quantity, amount)
				VALUES ($1, $2, $3::numeric, $4::numeric)`, in.GetClaimId(), l.OrderLineId, l.Measure.Quantity,
				l.Measure.Amount); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE order_line SET claimed_qty = claimed_qty + $2::numeric,
				claimed_amt = claimed_amt + $3::numeric WHERE id = $1`, l.OrderLineId, l.Measure.Quantity,
				l.Measure.Amount); err != nil {
				return err
			}
		}
		if err := m.ledgerFacts(ctx, tx, o.ID, o.LegalEntityID, ids, "claim"); err != nil {
			return err
		}
		c, err := getClaim(ctx, tx, in.GetClaimId(), false)
		if err != nil {
			return err
		}
		out = &salesv3.ClaimResponse{Claimed: true, Claim: c.proto()}
		return nil
	})
	return out, err
}

// Renew extends a pending claim's period.
func (m *Module) Renew(ctx context.Context, in *salesv3.RenewRequest) (*salesv3.RenewResponse, error) {
	var out *salesv3.RenewResponse
	err := m.claimTx(ctx, in.GetClaimId(), func(ctx context.Context, tx db.Tx, c claimRow) error {
		if c.State != "pending" || c.ExpiresAt == nil || !c.ExpiresAt.After(time.Now()) {
			return errClaim.New("only a pending claim within its period is renewed; this one is " + c.State)
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_claim SET expires_at = now() + make_interval(secs => $2), updated_at = now()
			WHERE id = $1`, c.ID, ClaimPeriod.Seconds()); err != nil {
			return err
		}
		c, err := getClaim(ctx, tx, c.ID, false)
		out = &salesv3.RenewResponse{Claim: c.proto()}
		return err
	})
	return out, err
}

// Confirm makes a pending claim confirmed before erp/billing numbers; a claim past its period expires now and is
// refused (BL05, BL26); confirming again answers the same (BL24).
func (m *Module) Confirm(ctx context.Context, in *salesv3.ConfirmRequest) (*salesv3.ConfirmResponse, error) {
	var out *salesv3.ConfirmResponse
	err := m.claimTx(ctx, in.GetClaimId(), func(ctx context.Context, tx db.Tx, c claimRow) error {
		switch c.State {
		case "confirmed", "formalized":
			out = &salesv3.ConfirmResponse{Confirmed: true, Claim: c.proto()}
			return nil
		case "pending":
			if c.ExpiresAt == nil || !c.ExpiresAt.After(time.Now()) {
				if err := m.giveBack(ctx, tx, c, "expired", "expire"); err != nil {
					return err
				}
				break
			}
			if _, err := tx.Exec(ctx, `UPDATE billing_claim SET state = 'confirmed', expires_at = NULL, updated_at = now()
				WHERE id = $1`, c.ID); err != nil {
				return err
			}
		}
		c, err := getClaim(ctx, tx, c.ID, false)
		out = &salesv3.ConfirmResponse{Confirmed: c.State == "confirmed", Claim: c.proto()}
		return err
	})
	return out, err
}

// Formalize records what was billed, at most the claim per line; the rest of the claim goes back. When every line is
// delivered and billed, the order completes and what is left of its branches is released.
func (m *Module) Formalize(ctx context.Context, in *salesv3.FormalizeRequest) (*salesv3.FormalizeResponse, error) {
	billed, err := claimLines(in.GetBilled(), "billed")
	if err != nil {
		return nil, err
	}
	var out *salesv3.FormalizeResponse
	err = m.claimTx(ctx, in.GetClaimId(), func(ctx context.Context, tx db.Tx, c claimRow) error {
		switch c.State {
		case "formalized":
			if !sameLines(c.Billed, billed) {
				return errContent.New("claim " + c.ID + " was formalized with other quantities")
			}
			out = &salesv3.FormalizeResponse{Claim: c.proto()}
			return nil
		case "confirmed":
		default:
			return errClaim.New("only a confirmed claim is formalized; this one is " + c.State)
		}
		claimed := map[string]*salesv3.Measure{}
		for _, l := range c.Lines {
			claimed[l.OrderLineId] = l.Measure
		}
		for _, b := range billed {
			cl, ok := claimed[b.OrderLineId]
			if !ok {
				return invalid("billed", "line "+b.OrderLineId+" is not in the claim")
			}
			var fits bool
			if err := tx.QueryRow(ctx, `SELECT $1::numeric <= $2::numeric AND $3::numeric <= $4::numeric`,
				b.Measure.Quantity, cl.Quantity, b.Measure.Amount, cl.Amount).Scan(&fits); err != nil {
				return err
			}
			if !fits {
				return invalid("billed", "line "+b.OrderLineId+" bills more than it claimed")
			}
		}
		for _, l := range c.Lines {
			q, a := "0", "0"
			for _, b := range billed {
				if b.OrderLineId == l.OrderLineId {
					q, a = b.Measure.Quantity, b.Measure.Amount
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE billing_claim_line SET billed_qty = $3::numeric, billed_amt = $4::numeric
				WHERE claim_id = $1 AND order_line_id = $2`, c.ID, l.OrderLineId, q, a); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE order_line SET claimed_qty = claimed_qty - $2::numeric,
				claimed_amt = claimed_amt - $3::numeric, billed_qty = billed_qty + $4::numeric, billed_amt = billed_amt + $5::numeric
				WHERE id = $1`, l.OrderLineId, l.Measure.Quantity, l.Measure.Amount, q, a); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_claim SET state = 'formalized', updated_at = now() WHERE id = $1`, c.ID); err != nil {
			return err
		}
		o, err := getOrder(ctx, tx, c.OrderID, false)
		if err != nil {
			return err
		}
		lines := make([]string, len(c.Lines))
		for i, l := range c.Lines {
			lines[i] = l.OrderLineId
		}
		if err := m.ledgerFacts(ctx, tx, o.ID, o.LegalEntityID, lines, "formalize"); err != nil {
			return err
		}
		if err := m.completeIfDone(ctx, tx, o.ID, caller(ctx)); err != nil {
			return err
		}
		c, err = getClaim(ctx, tx, c.ID, false)
		out = &salesv3.FormalizeResponse{Claim: c.proto()}
		return err
	})
	return out, err
}

// completeIfDone completes a confirmed order whose lines are all delivered and billed, and starts the release of what
// is left of its branches (docs/flows/confirmation.md, "完成时释放"); otherwise it publishes the order's new ledger.
func (m *Module) completeIfDone(ctx context.Context, tx db.Tx, orderID, actor string) error {
	o, err := getOrder(ctx, tx, orderID, false)
	if err != nil {
		return err
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM order_line WHERE order_id = $1 AND
		((kind = 'goods' AND delivered < quantity) OR billed_qty < quantity OR billed_amt < net))`, orderID).Scan(&done); err != nil {
		return err
	}
	if !done || o.State != "confirmed" {
		_, err := m.emitOrder(ctx, tx, orderID)
		return err
	}
	if err := m.setOrderState(ctx, tx, o, "completed", nil, actor, OpSysBilling); err != nil {
		return err
	}
	_, err = m.startBranchProcess(ctx, tx, o, "release", actor)
	return err
}

// Release gives a pending or confirmed claim back; releasing again answers the same.
func (m *Module) Release(ctx context.Context, in *salesv3.ReleaseRequest) (*salesv3.ReleaseResponse, error) {
	var out *salesv3.ReleaseResponse
	err := m.claimTx(ctx, in.GetClaimId(), func(ctx context.Context, tx db.Tx, c claimRow) error {
		switch c.State {
		case "pending", "confirmed":
			if err := m.giveBack(ctx, tx, c, "released", "release"); err != nil {
				return err
			}
		case "released", "expired":
		default:
			return errClaim.New("a formalized claim is reversed by a correction, not released")
		}
		c, err := getClaim(ctx, tx, c.ID, false)
		out = &salesv3.ReleaseResponse{Claim: c.proto()}
		return err
	})
	return out, err
}

// Reverse is a correction's return of formalized quantities (protocol 05): not in this version.
func (m *Module) Reverse(context.Context, *salesv3.ReverseRequest) (*salesv3.ReverseResponse, error) {
	return nil, errs.New(errs.ReasonCapabilityUnsupported, "corrections are not in this version of erp/sales")
}

// expireClaims ends the pending claims past their period, each under its order's lock (a confirm racing it is decided
// under the same lock).
func (m *Module) expireClaims(ctx context.Context, _ *work.Run) error {
	var due []string
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM billing_claim WHERE state = 'pending' AND expires_at <= now()
			ORDER BY expires_at LIMIT 500`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			due = append(due, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, id := range due {
		err := m.claimTx(ctx, id, func(ctx context.Context, tx db.Tx, c claimRow) error {
			if c.State != "pending" || c.ExpiresAt == nil || c.ExpiresAt.After(time.Now()) {
				return nil
			}
			return m.giveBack(ctx, tx, c, "expired", "expire")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Read answers orders for other components (OrderService): the data of erp.sales_order.changed.v1, as JSON (like
// the master data's source methods: the payload has nulls, which the canonical encoding refuses).
func (m *Module) Read(ctx context.Context, in *salesv3.ReadRequest) (*salesv3.ReadResponse, error) {
	if len(in.GetIds()) > MaxReadIDs {
		return nil, invalid("ids", fmt.Sprintf("at most %d IDs a call", MaxReadIDs))
	}
	out := &salesv3.ReadResponse{}
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		for _, id := range in.GetIds() {
			r := &salesv3.ReadResult{Id: id, State: salesv3.ReadState_READ_STATE_NOT_FOUND}
			if uuidForm.MatchString(id) {
				o, err := getOrder(ctx, tx, id, false)
				switch {
				case err == nil:
					data, err := json.Marshal(o)
					if err != nil {
						return err
					}
					r.State, r.Version, r.Data = salesv3.ReadState_READ_STATE_EXISTS, o.Version, data
				case !errors.Is(err, errNone):
					return err
				}
			}
			out.Results = append(out.Results, r)
		}
		return nil
	})
	return out, err
}
