package sales

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/work"
	creditv3 "github.com/brickKit/erp-credit/contract/v3/gen/go/be/erp/credit/v3"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
)

// Advancing a process: advance and its rounds, the decision table, the end, and the recovery work item.

type advanceArgs struct {
	ID string `json:"confirmation_id"`
}

// answer is what one command to a branch answered; known=false: the outcome is not known.
type answer struct {
	component string
	state     string
	reason    *string
	known     bool
}

// holdContent is what the branches hold: the goods lines merged by product and warehouse in base units, and the
// order's gross amount.
type holdContent struct {
	legalEntity, customer, currency, gross string
	lines                                  []*inventoryv3.HoldLine
}

func (m *Module) holdContentOf(ctx context.Context, orderID string) (holdContent, error) {
	var h holdContent
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text, customer_id::text, currency, trim_scale(net + tax)::text
			FROM sales_order WHERE id = $1`, orderID).Scan(&h.legalEntity, &h.customer, &h.currency, &h.gross); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT product_id::text, warehouse_id::text, trim_scale(sum(base_quantity))::text
			FROM order_line WHERE order_id = $1 AND kind = 'goods' GROUP BY 1, 2 ORDER BY 1, 2`, orderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l inventoryv3.HoldLine
			if err := rows.Scan(&l.ProductId, &l.WarehouseId, &l.Quantity); err != nil {
				return err
			}
			h.lines = append(h.lines, &l)
		}
		return rows.Err()
	})
	return h, err
}

// advance moves a process until it ends or a participant's answer is not known (errUnknown). Every round sends the
// command the state calls for to the branches that need it, in parallel, then records the answers and decides the
// next state in one transaction under the process's lock (protocol 04's tables).
func (m *Module) advance(ctx context.Context, id string) error {
	var content *holdContent
	for {
		p, err := m.process(ctx, id)
		if err != nil {
			return err
		}
		if terminal(p.State) {
			return nil
		}
		cmd, pending := "", []branch{}
		for _, b := range p.Branches {
			switch p.State {
			case "reserving":
				cmd = "hold"
				if b.State == bUnknown {
					pending = append(pending, b)
				}
			case "committing":
				cmd = "confirm"
				if b.State == bHeld || b.State == bUnknown {
					pending = append(pending, b)
				}
			default: // rejecting, cancelling
				cmd = "cancel"
				if !ended[b.State] {
					pending = append(pending, b)
				}
			}
		}
		if cmd == "hold" && len(pending) > 0 && content == nil {
			h, err := m.holdContentOf(ctx, p.OrderID)
			if err != nil {
				return err
			}
			content = &h
		}
		answers := make([]answer, len(pending))
		var wg sync.WaitGroup
		for i, b := range pending {
			wg.Add(1)
			go func() {
				defer wg.Done()
				answers[i] = m.send(ctx, p, b, cmd, content)
			}()
		}
		wg.Wait()
		moved, err := m.step(ctx, id, answers)
		if err != nil {
			return err
		}
		if !moved { // an answer is not known, or a participant answered what it answered before
			return errUnknown
		}
	}
}

// send gives one branch one command; an error asks the participant for the branch by its ID before giving up (the
// same ID is never replaced).
func (m *Module) send(ctx context.Context, p process, b branch, cmd string, h *holdContent) answer {
	a := answer{component: b.Component}
	conn, ok := m.rt.GRPC(b.Component)
	if !ok {
		return a
	}
	ref := func() (string, string, int64) { return "sales_order", p.OrderID, int64(p.Revision) }
	var state, reason string
	var err error
	switch b.Component {
	case Inventory:
		c := inventoryv3.NewReservationServiceClient(conn)
		var br *inventoryv3.Branch
		switch cmd {
		case "hold":
			kind, id, rev := ref()
			var res *inventoryv3.HoldResponse
			res, err = c.Hold(ctx, &inventoryv3.HoldRequest{BranchId: b.BranchID, LegalEntityId: h.legalEntity,
				Reference: &inventoryv3.Reference{Kind: kind, Id: id, Revision: rev}, Lines: h.lines}, calls.Idempotent())
			br = res.GetBranch()
		case "confirm":
			var res *inventoryv3.ConfirmResponse
			res, err = c.Confirm(ctx, &inventoryv3.ConfirmRequest{BranchId: b.BranchID}, calls.Idempotent())
			br = res.GetBranch()
		default:
			var res *inventoryv3.CancelResponse
			res, err = c.Cancel(ctx, &inventoryv3.CancelRequest{BranchId: b.BranchID}, calls.Idempotent())
			br = res.GetBranch()
		}
		if err != nil {
			var res *inventoryv3.QueryResponse
			if res, err = c.Query(ctx, &inventoryv3.QueryRequest{BranchId: b.BranchID}, calls.Idempotent()); err != nil {
				return a
			}
			br = res.GetBranch()
		}
		state, reason = br.GetState().String(), br.GetReason()
	case Credit:
		c := creditv3.NewReservationServiceClient(conn)
		var br *creditv3.Branch
		switch cmd {
		case "hold":
			kind, id, rev := ref()
			var res *creditv3.HoldResponse
			res, err = c.Hold(ctx, &creditv3.HoldRequest{BranchId: b.BranchID, PartnerId: h.customer,
				LegalEntityId: h.legalEntity, Currency: h.currency, Amount: h.gross,
				Reference: &creditv3.Reference{Kind: kind, Id: id, Revision: rev}}, calls.Idempotent())
			br = res.GetBranch()
		case "confirm":
			var res *creditv3.ConfirmResponse
			res, err = c.Confirm(ctx, &creditv3.ConfirmRequest{BranchId: b.BranchID}, calls.Idempotent())
			br = res.GetBranch()
		default:
			var res *creditv3.CancelResponse
			res, err = c.Cancel(ctx, &creditv3.CancelRequest{BranchId: b.BranchID}, calls.Idempotent())
			br = res.GetBranch()
		}
		if err != nil {
			var res *creditv3.QueryResponse
			if res, err = c.Query(ctx, &creditv3.QueryRequest{BranchId: b.BranchID}, calls.Idempotent()); err != nil {
				return a
			}
			br = res.GetBranch()
		}
		state, reason = br.GetState().String(), br.GetReason()
	default:
		return a
	}
	state = strings.ToLower(strings.TrimPrefix(state, "BRANCH_STATE_"))
	if state == "absent" || state == "unspecified" {
		return a // the command never arrived: the next round sends it again
	}
	a.state, a.known = state, true
	if reason != "" {
		a.reason = &reason
	}
	return a
}

// step records the answers (a branch's state only moves forward) and decides the process's next state; it reports
// whether anything changed.
func (m *Module) step(ctx context.Context, id string, answers []answer) (bool, error) {
	moved := false
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		moved = false
		p, err := getProcess(ctx, tx, id, true)
		if err != nil || terminal(p.State) {
			return err
		}
		for _, a := range answers {
			if !a.known {
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE confirmation_branch SET state = $3, reason = $4 WHERE confirmation_id = $1
				AND component = $2 AND state <> $3 AND state NOT IN ('released', 'expired', 'rejected', 'cancelled', 'consumed')
				AND NOT (state = 'committed' AND $3 = 'held')`, id, a.component, a.state, a.reason)
			if err != nil {
				return err
			}
			moved = moved || tag.RowsAffected() > 0
		}
		if p, err = getProcess(ctx, tx, id, false); err != nil {
			return err
		}
		next, reason := decide(p)
		if next == p.State {
			return nil
		}
		moved = true
		if reason == nil {
			reason = p.Reason
		}
		if _, err := tx.Exec(ctx, `UPDATE confirmation SET state = $2, reason = $3, updated_at = now() WHERE id = $1`,
			id, next, reason); err != nil {
			return err
		}
		if terminal(next) {
			p.State, p.Reason = next, reason
			return m.finish(ctx, tx, p)
		}
		return nil
	})
	return moved, err
}

// decide is protocol 04's process table given the branches' last states.
func decide(p process) (string, *string) {
	all := func(ok func(string) bool) bool {
		for _, b := range p.Branches {
			if !ok(b.State) {
				return false
			}
		}
		return true
	}
	first := func(bad func(string) bool) *branch {
		for i, b := range p.Branches {
			if bad(b.State) {
				return &p.Branches[i]
			}
		}
		return nil
	}
	why := func(s string) *string { return &s }
	switch p.State {
	case "reserving":
		if b := first(func(s string) bool { return ended[s] }); b != nil {
			if b.State == "expired" {
				return "rejecting", why("processing_timeout")
			}
			if b.Reason != nil {
				return "rejecting", b.Reason
			}
			return "rejecting", why(b.State)
		}
		if all(func(s string) bool { return s == bHeld || s == bCommitted }) {
			return "committing", nil
		}
	case "committing":
		if b := first(func(s string) bool { return ended[s] }); b != nil {
			if b.State == "expired" {
				return "rejecting", why("processing_timeout")
			}
			return "rejecting", why(b.State)
		}
		if all(func(s string) bool { return s == bCommitted }) {
			return "confirmed", nil
		}
	case "rejecting":
		if all(func(s string) bool { return ended[s] }) {
			return "rejected", nil
		}
	case "cancelling":
		if all(func(s string) bool { return ended[s] }) {
			return "cancelled", nil
		}
	}
	return p.State, nil
}

// finish commits what a process's end means for the order, in the transaction that ended it (RS06).
func (m *Module) finish(ctx context.Context, tx db.Tx, p process) error {
	o, err := getOrder(ctx, tx, p.OrderID, true)
	if err != nil {
		return err
	}
	switch p.Kind {
	case "release":
		return nil
	case "cancel":
		if o.State != "confirmed" {
			return nil
		}
		return m.setOrderState(ctx, tx, o, "cancelled", nil, p.Actor, OpOrderCancel)
	}
	if o.State != "confirming" || deref(o.ConfirmationID) != p.ID {
		return nil
	}
	switch p.State {
	case "confirmed":
		lines, err := tx.Query(ctx, `UPDATE order_line SET billable_qty = quantity, billable_amt = net
			WHERE order_id = $1 AND kind = 'service' RETURNING id::text`, o.ID)
		if err != nil {
			return err
		}
		var service []string
		for lines.Next() {
			var id string
			if err := lines.Scan(&id); err != nil {
				lines.Close()
				return err
			}
			service = append(service, id)
		}
		lines.Close()
		if err := m.ledgerFacts(ctx, tx, o.ID, o.LegalEntityID, service, "service_confirmed"); err != nil {
			return err
		}
		if err := m.setOrderState(ctx, tx, o, "confirmed", nil, p.Actor, OpOrderConfirm); err != nil {
			return err
		}
	case "rejected":
		if err := m.setOrderState(ctx, tx, o, "draft", p.Reason, p.Actor, OpOrderConfirm); err != nil {
			return err
		}
	case "cancelled":
		if err := m.setOrderState(ctx, tx, o, "cancelled", p.Reason, p.Actor, OpOrderCancel); err != nil {
			return err
		}
	}
	return m.emit(ctx, tx, ConfirmationEnded, p.ID, 1, o.LegalEntityID, map[string]any{"fact_id": p.ID,
		"order_id": o.ID, "outcome": p.State, "reason": p.Reason, "occurred_at": canon.Time(time.Now())})
}

func (m *Module) setOrderState(ctx context.Context, tx db.Tx, o Order, state string, reason *string, actor, op string) error {
	if _, err := tx.Exec(ctx, `UPDATE sales_order SET state = $2, reason = $3, version = version + 1, updated_at = now()
		WHERE id = $1`, o.ID, state, reason); err != nil {
		return err
	}
	after, err := m.emitOrder(ctx, tx, o.ID)
	if err != nil {
		return err
	}
	return m.change(ctx, tx, actor, op, Orders, o.ID, after)
}

// advanceJob is the recovery of a process (protocol 01, idempotent_external): one per process, enqueued with it.
// While the request's synchronous path still has the process, it waits.
func (m *Module) advanceJob(ctx context.Context, r *work.Run) error {
	var a advanceArgs
	if err := json.Unmarshal(r.Args, &a); err != nil {
		return work.Permanent(err)
	}
	p, err := m.process(ctx, a.ID)
	if err != nil {
		if errors.Is(err, errNone) {
			return work.Permanent(err)
		}
		return err
	}
	if terminal(p.State) {
		return nil
	}
	if p.young {
		return work.Wait(errors.New("the request's synchronous path still has the process"))
	}
	return m.advance(ctx, a.ID)
}

// advanceDead runs when the recovery used its attempts: a process still reserving or committing gives up with
// "dependency_unavailable" and releases its branches through a new work item; a release that keeps failing stays
// dead for an operator.
func (m *Module) advanceDead(ctx context.Context, tx db.Tx, item work.Item, _ error) error {
	var a advanceArgs
	if err := json.Unmarshal(item.Args, &a); err != nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE confirmation SET state = 'rejecting', reason = 'dependency_unavailable',
		updated_at = now() WHERE id = $1 AND state IN ('reserving', 'committing')`, a.ID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = m.rt.Work.Enqueue(ctx, tx, jobAdvance, a)
	return err
}
