package recoverycheck

import (
	"context"
	"fmt"
	"time"

	confirmations "github.com/brickKit/erp-sales/v3/internal/confirmations"
	core "github.com/brickKit/erp-sales/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/recovery"
	"github.com/jackc/pgx/v5"
)

// CheckReservations is protocol 04's check after a full recovery (protocol 15, step 8). This component is the
// coordinator, so it reports for both sides (P14.2): its processes against the participants' branches.
const CheckReservations = "erp.sales.reservations"

// endedWindow is how far back an ended (rejected or cancelled) process is compared with its participants: a hold that
// outlived its process shows up within days; older ones were compared by earlier checks.
const endedWindow = 30 * 24 * time.Hour

// settled is a branch of an order's latest ended process and what the participant should say about it.
type settled struct {
	order, process, state, component, branch string
}

// expect is whether a participant's branch state fits the process state: a confirmed order's branches are committed
// (or consumed by its deliveries); a rejected or cancelled process holds nothing any more.
func expect(processState, branchState string) bool {
	if processState == "confirmed" {
		return branchState == confirmations.BCommitted || branchState == "consumed"
	}
	return confirmations.Ended[branchState] || branchState == "absent"
}

// ReservationsCheck lists, from this database, the processes and deliveries in flight at the recovery point and the
// branches the settled processes name, then asks each participant for its branch outside any transaction.
func ReservationsCheck(m *core.Base, ctx context.Context, read recovery.Read) (recovery.Result, error) {
	var diffs []recovery.Difference
	var branches []settled
	err := read(ctx, func(ctx context.Context, tx db.Tx) error {
		inFlight, err := collectDiffs(ctx, tx, `SELECT 'confirmation', id::text, kind || ' ' || state || ' (order ' || order_id || ')'
			FROM confirmation WHERE state IN ('reserving', 'committing', 'rejecting', 'cancelling')
			UNION ALL
			SELECT 'delivery', id::text, 'issuing (order ' || order_id || '): the issue at the inventory is not known here'
			FROM delivery WHERE state = 'issuing'
			ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		diffs = inFlight
		rows, err := tx.Query(ctx, `WITH latest AS (
				SELECT DISTINCT ON (order_id) id, order_id, state, updated_at FROM confirmation
				ORDER BY order_id, started_at DESC, id DESC)
			SELECT l.order_id::text, l.id::text, l.state, b.component, b.branch_id::text
			FROM latest l JOIN sales_order o ON o.id = l.order_id JOIN confirmation_branch b ON b.confirmation_id = l.id
			WHERE l.state = 'confirmed' AND o.state = 'confirmed'
			   OR l.state IN ('rejected', 'cancelled') AND l.updated_at > now() - make_interval(secs => $1)
			ORDER BY l.order_id, b.component`, endedWindow.Seconds())
		if err != nil {
			return err
		}
		branches, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (settled, error) {
			var s settled
			return s, r.Scan(&s.order, &s.process, &s.state, &s.component, &s.branch)
		})
		return err
	})
	if err != nil {
		return recovery.Result{}, err
	}
	mismatch, unknown := len(diffs) > 0, false
	for _, s := range branches {
		state, _, err := confirmations.QueryBranch(m, ctx, s.component, s.branch)
		switch {
		case err != nil:
			unknown = true
			diffs = append(diffs, recovery.Difference{ObjectType: "branch", ObjectID: s.branch,
				Detail: fmt.Sprintf("order %s, process %s %s: %s could not be asked: %v", s.order, s.process, s.state, s.component, err)})
		case !expect(s.state, state):
			mismatch = true
			diffs = append(diffs, recovery.Difference{ObjectType: "branch", ObjectID: s.branch,
				Detail: fmt.Sprintf("order %s, process %s %s: %s says %s", s.order, s.process, s.state, s.component, state)})
		}
	}
	res := recovery.Result{Outcome: recovery.Pass, Differences: diffs}
	switch {
	case mismatch:
		res.Outcome = recovery.Diff
		res.Disposal = "进行中的过程与出库中的交货：开放写入之后由恢复工作项按分支身份查询参与方并推进，不另行处理；" +
			"已结束的过程与参与方不一致的分支：按协议 04 人工核对（已确认而参与方没有占住的，重新确认或取消订单；已取消而仍占着的，向参与方取消该分支）"
	case unknown:
		res.Outcome = recovery.Unprovable
		res.Disposal = "参与方问不到：等它在部署里就绪后重做检查"
	}
	return res, nil
}

// collectDiffs reads (object type, object id, detail) rows into differences.
func collectDiffs(ctx context.Context, tx db.Tx, q string, args ...any) ([]recovery.Difference, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (recovery.Difference, error) {
		var d recovery.Difference
		return d, r.Scan(&d.ObjectType, &d.ObjectID, &d.Detail)
	})
}
