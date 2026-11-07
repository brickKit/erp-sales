//go:build integration

package itest

import (
	"regexp"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// deliveries: the delivery ID is the consumption ID (RS19); the billable amount grows by rounding and the last
// delivery makes it exactly the line amount; a refused issue fails the delivery; an unanswered one is posted later.
// SO12: a confirmed order is delivered.
func (fx *fixture) deliveries() {
	t, inv := fx.t, fx.inv
	a, b := fx.orders["A"], fx.orders["B"]
	invA := branchOf(fx.procs["A"], core.Inventory)
	lineA := at(a, "lines", 0, "id").(string)
	deliver := func(order, line, qty string) (int, map[string]any) {
		return fx.alice.do("POST", "/api/sales/deliveries", map[string]any{"order_id": order,
			"lines": []any{map[string]any{"order_line_id": line, "quantity": qty}}})
	}
	code, dn := deliver(a["id"].(string), lineA, "1")
	require.Equal(t, 200, code, dn)
	assert.Equal(t, "posted", dn["state"])
	assert.Regexp(t, regexp.MustCompile(`^DN-SH-`+fx.year+`-\d{6}$`), dn["number"])
	issued := inv.issue(dn["id"].(string))
	require.NotNil(t, issued)
	assert.Equal(t, invA, issued.BranchId)
	assert.Equal(t, "12", issued.Lines[0].Quantity)
	a = fx.get("/api/sales/orders/" + a["id"].(string))
	assert.Equal(t, "1", at(a, "lines", 0, "delivered_quantity"))
	assert.Equal(t, map[string]any{"quantity": "1", "amount": "120"}, at(a, "lines", 0, "billable"))
	code, over := deliver(a["id"].(string), lineA, "2")
	assert.Equal(t, 409, code, "more than is left to deliver")
	assert.Equal(t, "STATE_CONFLICT", over["reason"])
	inv.setIssue("reject")
	code, failed := deliver(a["id"].(string), lineA, "1")
	require.Equal(t, 200, code, failed)
	assert.Equal(t, "failed", failed["state"])
	assert.Contains(t, failed["reason"], "insufficient_batch")
	inv.setIssue("")
	code, dn2 := deliver(a["id"].(string), lineA, "1")
	require.Equal(t, 200, code, dn2)
	a = fx.get("/api/sales/orders/" + a["id"].(string))
	assert.Equal(t, map[string]any{"quantity": "2", "amount": "240"}, at(a, "lines", 0, "billable"))
	fx.orders["A"] = a

	inv.setIssue("down")
	code, dnB := deliver(b["id"].(string), at(b, "lines", 0, "id").(string), "1")
	require.Equal(t, 200, code, dnB)
	assert.Equal(t, "issuing", dnB["state"])
	inv.setIssue("")
	require.Eventually(t, func() bool {
		return fx.get("/api/sales/deliveries/" + dnB["id"].(string))["state"] == "posted"
	}, 60*time.Second, 300*time.Millisecond)
}
