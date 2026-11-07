//go:build integration

package itest

import (
	"context"
	"time"

	orders "github.com/brickKit/erp-sales/v3/internal/orders"

	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tiering (assembly protocol 11, A154, A155, TR10, TR30): two orders that ended long ago are sealed with every level
// of their aggregate once an open confirmation process no longer holds them back, copied and released; read by their
// keys they are as they were; the default list keeps an order created long ago that has not ended and leaves out the
// archived ones; the history reads them.
func (fx *fixture) tiering() {
	t, ctx := fx.t, fx.ctx
	cat := fx.rt.Tiering
	require.NotNil(t, cat)
	cat.Grace, cat.Backups = 0, func(context.Context) (time.Time, bool, error) { return time.Now().Add(time.Hour), true, nil }
	ended := []map[string]any{fx.order(fx.goods("2")), fx.order(fx.goods("3"))}
	open := fx.order(fx.goods("1"))
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := fx.admin.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	for i, o := range ended {
		state := []string{"completed", "cancelled"}[i]
		exec("UPDATE "+itSchema+".sales_order SET state = $2, created_at = now() - interval '500 days', "+
			"updated_at = now() - interval '400 days' WHERE id = $1", o["id"], state)
	}
	exec("UPDATE "+itSchema+".sales_order SET created_at = now() - interval '500 days' WHERE id = $1", open["id"])
	recent := fx.order(fx.goods("1")) // created long ago, ended a month ago: online still, but past the default range
	exec("UPDATE "+itSchema+".sales_order SET state = 'cancelled', created_at = now() - interval '500 days', "+
		"updated_at = now() - interval '30 days' WHERE id = $1", recent["id"])
	// a release process still running on the first order's reservations holds the grouping back (TR30)
	process := ids.New().String()
	exec("INSERT INTO "+itSchema+".confirmation (id, order_id, revision, state, kind, actor) VALUES ($1, $2, 1, "+
		"'reserving', 'release', 'test')", process, ended[0]["id"])
	before := map[string]map[string]any{}
	for _, o := range ended {
		before[o["id"].(string)] = fx.get("/api/sales/orders/" + o["id"].(string))
	}

	groups, err := cat.Groupings(ctx, orders.OrderClass)
	require.NoError(t, err)
	require.Len(t, groups, 1, "one legal entity, one month long ago")
	held, err := cat.Seal(ctx, orders.OrderClass, groups[0])
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{ended[0]["id"].(string): {orders.WhyConfirmationOpen}}, held.Blocked, "TR30")
	assert.Zero(t, held.Members, "the grouping stays open")
	exec("UPDATE "+itSchema+".confirmation SET state = 'confirmed' WHERE id = $1", process)

	sealed, err := cat.Seal(ctx, orders.OrderClass, groups[0])
	require.NoError(t, err)
	require.Equal(t, 2, sealed.Members)
	_, err = cat.Transfer(ctx, sealed.Unit)
	require.NoError(t, err)
	// published, its online rows still there: a release deletes them in batches, children first; in between, the
	// order reads from the archive whole, never as the header with no lines left online (3.0.37)
	tx, err := fx.admin.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SELECT set_config('be.tier_unit', $1, true)", sealed.Unit)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "DELETE FROM "+itSchema+".order_line WHERE order_id = $1", ended[0]["id"])
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	assert.Equal(t, before[ended[0]["id"].(string)], fx.get("/api/sales/orders/"+ended[0]["id"].(string)),
		"halfway through a release: read whole from the archive")
	why, err := cat.Release(ctx, sealed.Unit)
	require.NoError(t, err)
	require.Empty(t, why)
	var online int
	require.NoError(t, fx.admin.QueryRow(ctx, "SELECT count(*) FROM "+itSchema+".order_line WHERE order_id = ANY($1::uuid[])",
		[]string{ended[0]["id"].(string), ended[1]["id"].(string)}).Scan(&online))
	require.Zero(t, online, "released with every level")

	for id, o := range before {
		assert.Equal(t, o, fx.get("/api/sales/orders/"+id), "read by its key from the archive, as it was")
	}
	list := fx.get("/api/sales/orders?legal_entity_id=" + fx.le + "&limit=200")
	listed := map[string]bool{}
	for _, o := range list["orders"].([]any) {
		listed[o.(map[string]any)["id"].(string)] = true
	}
	assert.True(t, listed[open["id"].(string)], "TR10: created long ago, not ended: in the default list")
	for id := range before {
		assert.False(t, listed[id], "archived: not in the default list")
	}
	assert.False(t, listed[recent["id"].(string)], "TR10: ended, created long ago: past the default range")
	hist := fx.get("/api/sales/orders?history=true&legal_entity_id=" + fx.le)
	found := map[string]map[string]any{}
	for _, o := range hist["orders"].([]any) {
		found[o.(map[string]any)["id"].(string)] = o.(map[string]any)
	}
	for id, o := range before {
		assert.Equal(t, o, found[id], "the history reads the archived order")
	}
	assert.Contains(t, found, open["id"].(string), "and the online ones")
	assert.Contains(t, found, recent["id"].(string), "and those past the default range")
	cancelled := fx.get("/api/sales/orders?history=true&state=cancelled&legal_entity_id=" + fx.le)
	var archivedCancelled []string
	for _, o := range cancelled["orders"].([]any) {
		if id := o.(map[string]any)["id"].(string); before[id] != nil {
			archivedCancelled = append(archivedCancelled, id)
		}
	}
	assert.Equal(t, []string{ended[1]["id"].(string)}, archivedCancelled, "the filters select the archived orders too")
}
