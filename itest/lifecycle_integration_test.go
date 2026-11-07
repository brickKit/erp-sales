//go:build integration

package itest

import (
	"github.com/brickKit/be-sdk-go/v3/ids"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// lifecycle locks the cells of the order's lifecycle that the business flows do not reach: a draft is cancelled at
// once and cancelling it again answers the same; a cancelled order is neither changed nor confirmed; a draft is
// neither delivered nor claimed; a completed order is neither cancelled, delivered nor claimed; a confirmed order
// with a delivery is not cancelled; an order whose cancel process is under way is neither delivered nor claimed, and
// cancelling it again starts no second process.
func (fx *fixture) lifecycle() {
	t, ctx := fx.t, fx.ctx
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, core.Billing).ClientConfig(core.ID))))
	require.NoError(t, err)
	defer conn.Close()
	src := salesv3.NewBillingSourceServiceClient(conn)
	id := func(o map[string]any) string { return o["id"].(string) }
	claim := func(o map[string]any) string {
		got, err := src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: ids.New().String(), OrderId: id(o),
			Lines: []*salesv3.ClaimLine{{OrderLineId: at(o, "lines", 0, "id").(string),
				Measure: &salesv3.Measure{Quantity: "1", Amount: "1"}}}})
		require.NoError(t, err)
		require.False(t, got.GetClaimed())
		return got.GetProblems()[0].GetReason()
	}
	deliver := func(o map[string]any) (int, map[string]any) {
		return fx.alice.do("POST", "/api/sales/deliveries", map[string]any{"order_id": id(o),
			"lines": []any{map[string]any{"order_line_id": at(o, "lines", 0, "id"), "quantity": "1"}}})
	}
	cancel := func(o map[string]any) (int, map[string]any) {
		return fx.alice.do("POST", "/api/sales/orders/"+id(o)+"/cancel", nil)
	}
	refused := func(code int, body map[string]any, detail string) {
		t.Helper()
		assert.Equal(t, 409, code, body)
		assert.Equal(t, "STATE_CONFLICT", body["reason"], body)
		assert.Equal(t, detail, body["detail"], body)
	}

	// a draft: cancelled at once, again answers the same; then neither changed nor confirmed
	d := fx.order(fx.goods("1"))
	code, got := cancel(d)
	require.Equal(t, 200, code, got)
	assert.Equal(t, "cancelled", got["state"])
	code, got = cancel(d)
	require.Equal(t, 200, code, got)
	assert.Equal(t, "cancelled", got["state"], "cancelling a cancelled order answers the same")
	code, got = fx.alice.do("PATCH", "/api/sales/orders/"+id(d), map[string]any{"lines": []any{fx.goods("2")}})
	refused(code, got, "only a draft order changes; this one is cancelled (revisions come later)")
	code, got = fx.alice.do("POST", "/api/sales/orders/"+id(d)+"/confirm", nil)
	refused(code, got, "only a draft order is confirmed; this one is cancelled")

	// a draft is neither delivered nor claimed
	e := fx.order(fx.goods("1"))
	code, got = deliver(e)
	refused(code, got, "only a confirmed order is delivered; this one is draft")
	assert.Equal(t, "order_not_billable", claim(e))
	code, got = cancel(e)
	require.Equal(t, 200, code, got)

	// a completed order (A, completed by the billing source step)
	a := fx.get("/api/sales/orders/" + id(fx.orders["A"]))
	require.Equal(t, "completed", a["state"])
	code, got = cancel(a)
	refused(code, got, "a completed order is not cancelled")
	code, got = deliver(a)
	refused(code, got, "only a confirmed order is delivered; this one is completed")
	assert.Equal(t, "order_not_billable", claim(a))

	// a confirmed order with a delivery (B) is not cancelled
	b := fx.get("/api/sales/orders/" + id(fx.orders["B"]))
	require.Equal(t, "confirmed", b["state"])
	code, got = cancel(b)
	refused(code, got, "the order has deliveries or billing: what was delivered goes back through a return; "+
		"closing what is not delivered comes later")

	// an order whose cancel process is under way: a cancel process is put in place by hand, so it stays open
	f := fx.order(fx.goods("1"))
	require.Equal(t, "confirmed", fx.confirm(f)["outcome"])
	f = fx.get("/api/sales/orders/" + id(f))
	proc := ids.New().String()
	_, err = fx.admin.Exec(ctx, `INSERT INTO `+itSchema+`.confirmation (id, order_id, revision, state, kind, actor)
		VALUES ($1, $2, 1, 'cancelling', 'cancel', 'itest')`, proc, id(f))
	require.NoError(t, err)
	code, got = deliver(f)
	refused(code, got, "the order is being cancelled")
	assert.Equal(t, "order_not_billable", claim(f))
	code, got = cancel(f)
	require.Equal(t, 200, code, got)
	assert.Equal(t, "confirmed", got["state"], "the order stays confirmed until its cancel process ends")
	var processes int
	require.NoError(t, fx.admin.QueryRow(ctx, `SELECT count(*) FROM `+itSchema+`.confirmation WHERE order_id = $1
		AND kind = 'cancel'`, id(f)).Scan(&processes))
	assert.Equal(t, 1, processes, "cancelling again starts no second process")
	_, err = fx.admin.Exec(ctx, `DELETE FROM `+itSchema+`.confirmation WHERE id = $1`, proc)
	require.NoError(t, err)
}
