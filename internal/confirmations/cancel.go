package confirmations

import (
	"context"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	documents "github.com/brickKit/erp-sales/v3/internal/documents"
	orders "github.com/brickKit/erp-sales/v3/internal/orders"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
)

// Cancelling an order and releasing what is left of its branches: processes of kind cancel and release.

// StartBranchProcess starts a cancel or release process over the confirmed process's branches.
func StartBranchProcess(m *core.Base, ctx context.Context, tx db.Tx, o core.Order, kind, actor string) (string, error) {
	id := ids.New().String()
	if _, err := tx.Exec(ctx, `INSERT INTO confirmation (id, order_id, revision, state, kind, reason, actor)
		VALUES ($1, $2, $3, 'cancelling', $4, $4, $5)`, id, o.ID, o.Revision, kind, actor); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO confirmation_branch (confirmation_id, component, branch_id)
		SELECT $1, component, branch_id FROM confirmation_branch WHERE confirmation_id = $2`, id, Deref(o.ConfirmationID)); err != nil {
		return "", err
	}
	_, err := m.RT.Work.Enqueue(ctx, tx, JobAdvance, advanceArgs{ID: id})
	return id, err
}

// OpenCancel reports whether a confirmed order is being cancelled (nothing more is delivered or billed then).
func OpenCancel(ctx context.Context, tx db.Tx, orderID string) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM confirmation WHERE order_id = $1 AND kind = 'cancel'
		AND state = 'cancelling')`, orderID).Scan(&open)
	return open, err
}

// CancelOrder cancels a draft at once, a confirming order through its process (cancelling, RS04) and a confirmed order
// with nothing delivered or billed through a cancel process; it follows a process for the synchronous budget.
func CancelOrder(m *core.Base, c *httpx.Context) error {
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	cur, err := documents.Order(m, c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := core.Check(c, core.OpOrderCancel, core.DocFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID))
	if err != nil {
		return err
	}
	var procID string
	out, replayed, err := idem.Do(ctx, m.RT.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		procID = ""
		o, err := core.GetOrder(ctx, tx, cur.ID, true)
		if err != nil {
			return idem.Outcome{}, err
		}
		switch o.State {
		case "cancelled":
		case "draft":
			if err := orders.SetOrderState(m, ctx, tx, o, "cancelled", nil, actor, core.OpOrderCancel); err != nil {
				return idem.Outcome{}, err
			}
		case "confirming":
			if _, err := tx.Exec(ctx, `UPDATE confirmation SET state = 'cancelling', reason = 'cancelled', actor = $2,
				updated_at = now() WHERE id = $1 AND state IN ('reserving', 'committing', 'rejecting')`,
				Deref(o.ConfirmationID), actor); err != nil {
				return idem.Outcome{}, err
			}
			procID = Deref(o.ConfirmationID)
		case "confirmed":
			var started bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM delivery WHERE order_id = $1 AND state <> 'failed')
				OR EXISTS (SELECT 1 FROM order_line WHERE order_id = $1 AND (claimed_qty > 0 OR billed_qty > 0
				OR claimed_amt > 0 OR billed_amt > 0))`, o.ID).Scan(&started); err != nil {
				return idem.Outcome{}, err
			}
			if started {
				return idem.Outcome{}, core.ErrState.New("the order has deliveries or billing: what was delivered goes back " +
					"through a return; closing what is not delivered comes later")
			}
			open, err := OpenCancel(ctx, tx, o.ID)
			if err != nil || open {
				return documents.IDResult(o.ID), err
			}
			if procID, err = StartBranchProcess(m, ctx, tx, o, "cancel", actor); err != nil {
				return idem.Outcome{}, err
			}
		default:
			return idem.Outcome{}, core.ErrState.New("a " + o.State + " order is not cancelled")
		}
		return documents.IDResult(o.ID), nil
	})
	if err != nil {
		return err
	}
	if procID != "" && !replayed {
		sync, cancel := context.WithTimeout(ctx, core.SyncBudget)
		_ = advance(m, sync, procID)
		cancel()
	}
	o, err := documents.Order(m, ctx, documents.ResultID(out))
	if err != nil {
		return err
	}
	return c.JSON(200, o)
}
