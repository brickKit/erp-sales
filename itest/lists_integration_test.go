//go:build integration

package itest

import (
	"net/url"

	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// lists: the keyword finds an order by its number; to_bill lists the orders with billable left.
func (fx *fixture) lists() {
	t := fx.t
	a, b := fx.orders["A"], fx.orders["B"]
	page := fx.get("/api/sales/orders?q=" + url.QueryEscape(a["number"].(string)))
	require.Len(t, page["orders"], 1)
	assert.Equal(t, a["id"], at(page, "orders", 0, "id"))
	billable := map[any]bool{}
	for _, o := range fx.get("/api/sales/orders?to_bill=true")["orders"].([]any) {
		billable[at(o, "id")] = true
	}
	assert.True(t, billable[b["id"]], "order B has its delivered line to bill")
	assert.False(t, billable[a["id"]], "order A is billed")
	assert.Len(t, fx.get("/api/sales/deliveries?order_id=" + a["id"].(string))["deliveries"], 3)
}
