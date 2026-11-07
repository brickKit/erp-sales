//go:build integration

package itest

import (
	"context"
	"strconv"
	"testing"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	recoverycheck "github.com/brickKit/erp-sales/v3/internal/recoverycheck"

	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/recoverypb"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// checksStory is a full recovery's checks against this member (be-protocol chapter 14): the recovery tool only; the
// audit of a business change names the object's version after it (P14.3), and the owner answers whether the object
// is still there at that version.
func checksStory(t *testing.T, w *sectest.World, admin *pgx.Conn, typ, id string) {
	ctx := context.Background()
	dial := func(as string) recoverypb.CheckServiceClient {
		conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(w.TLS(t, as).ClientConfig(core.ID))))
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		return recoverypb.NewCheckServiceClient(conn)
	}
	_, err := dial("mdm/partner").Check(ctx, &recoverypb.CheckRequest{Recovery: "r"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the recovery tool only: %v", err)
	cs := dial("ops/recover")
	checked, err := cs.Check(ctx, &recoverypb.CheckRequest{Recovery: "r"})
	require.NoError(t, err)
	assert.NotEmpty(t, checked.GetResults())

	var version string
	require.NoError(t, admin.QueryRow(ctx, "SELECT target->>'version' FROM "+itSchema+".be_audit_log WHERE target->>'type' = $1 "+
		"AND target->>'id' = $2 ORDER BY ord DESC LIMIT 1", typ, id).Scan(&version), "the audit names the version")
	v, err := strconv.ParseInt(version, 10, 64)
	require.NoError(t, err)
	present, err := cs.Present(ctx, &recoverypb.PresentRequest{Recovery: "r", Objects: []*recoverypb.ObjectRef{
		{Type: typ, Id: id, Version: version}, {Type: typ, Id: id, Version: strconv.FormatInt(v+1, 10)},
		{Type: typ, Id: ids.New().String(), Version: "1"}}})
	require.NoError(t, err)
	var answers []string
	for _, a := range present.GetAnswers() {
		answers = append(answers, a.GetAnswer())
	}
	assert.Equal(t, []string{"present", "older", "absent"}, answers)
}

// reservations runs the recovery's checks and answers erp.sales.reservations.
func (fx *fixture) reservations() *recoverypb.CheckResult {
	fx.t.Helper()
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(fx.t, "ops/recover").ClientConfig(core.ID))))
	require.NoError(fx.t, err)
	defer conn.Close()
	res, err := recoverypb.NewCheckServiceClient(conn).Check(fx.ctx, &recoverypb.CheckRequest{Recovery: "r"})
	require.NoError(fx.t, err)
	for _, r := range res.GetResults() {
		if r.GetCheck() == recoverycheck.CheckReservations {
			return r
		}
	}
	fx.t.Fatal("no " + recoverycheck.CheckReservations)
	return nil
}

func detailsOf(r *recoverypb.CheckResult) map[string]string {
	out := map[string]string{}
	for _, d := range r.GetDifferences() {
		out[d.GetObjectId()] = d.GetDetail()
	}
	return out
}

// recoveryChecks: the presence of what this member audits, and protocol 04's check after a full recovery: the
// processes and deliveries in flight at the recovery point, and the participants' branches of settled processes.
func (fx *fixture) recoveryChecks() {
	t := fx.t
	var order string
	require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT id::text FROM "+itSchema+".sales_order WHERE state = 'confirmed' LIMIT 1").Scan(&order))
	t.Run("recovery checks", func(t *testing.T) { checksStory(t, fx.w, fx.admin, core.Orders, order) })

	base := fx.reservations()
	require.Equal(t, "pass", base.GetOutcome(), "after the flows everything is settled and the stand-ins agree: %v", base.GetDifferences())

	// a confirmed order whose inventory branch the participant no longer holds
	var conf, branch string
	require.NoError(t, fx.admin.QueryRow(fx.ctx, `SELECT c.id::text, b.branch_id::text FROM `+itSchema+`.confirmation c
		JOIN `+itSchema+`.confirmation_branch b ON b.confirmation_id = c.id
		WHERE c.order_id = $1 AND c.state = 'confirmed' AND b.component = 'erp/inventory' ORDER BY c.started_at DESC LIMIT 1`, order).Scan(&conf, &branch))
	was := fx.inv.state(branch)
	fx.inv.mu.Lock()
	fx.inv.branches[branch] = "released"
	fx.inv.mu.Unlock()
	got := fx.reservations()
	assert.Equal(t, "diff", got.GetOutcome())
	assert.Contains(t, detailsOf(got)[branch], "erp/inventory says released")
	fx.inv.mu.Lock()
	fx.inv.branches[branch] = "consumed" // delivered in full: fits a confirmed order
	fx.inv.mu.Unlock()
	assert.Equal(t, "pass", fx.reservations().GetOutcome())
	fx.inv.mu.Lock()
	fx.inv.branches[branch] = was
	fx.inv.mu.Unlock()

	// a rejected process whose branch a participant still holds
	var rejected, held, comp string
	require.NoError(t, fx.admin.QueryRow(fx.ctx, `SELECT c.id::text, b.branch_id::text, b.component FROM `+itSchema+`.confirmation c
		JOIN `+itSchema+`.confirmation_branch b ON b.confirmation_id = c.id
		WHERE c.state IN ('rejected', 'cancelled') AND c.started_at = (SELECT max(started_at) FROM `+itSchema+`.confirmation
			WHERE order_id = c.order_id) ORDER BY c.started_at DESC LIMIT 1`).Scan(&rejected, &held, &comp))
	p := fx.inv.participant
	if comp == core.Credit {
		p = fx.cred.participant
	}
	before := p.state(held)
	p.mu.Lock()
	p.branches[held] = "committed"
	p.mu.Unlock()
	got = fx.reservations()
	assert.Equal(t, "diff", got.GetOutcome())
	assert.Contains(t, detailsOf(got)[held], comp+" says committed")
	p.mu.Lock()
	p.branches[held] = before
	p.mu.Unlock()

	// a process in flight at the recovery point
	_, err := fx.admin.Exec(fx.ctx, "UPDATE "+itSchema+".confirmation SET state = 'committing' WHERE id = $1", conf)
	require.NoError(t, err)
	got = fx.reservations()
	assert.Equal(t, "diff", got.GetOutcome())
	assert.Contains(t, detailsOf(got)[conf], "committing")
	_, err = fx.admin.Exec(fx.ctx, "UPDATE "+itSchema+".confirmation SET state = 'confirmed' WHERE id = $1", conf)
	require.NoError(t, err)

	// a participant that cannot be asked: not provable
	fx.inv.set("query", "down")
	got = fx.reservations()
	assert.Equal(t, "unprovable", got.GetOutcome())
	assert.Contains(t, detailsOf(got)[branch], "could not be asked")
	fx.inv.set("query", "")
	assert.Equal(t, "pass", fx.reservations().GetOutcome())
}
