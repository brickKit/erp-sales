//go:build integration

package itest

import (
	"regexp"
	"time"

	"github.com/brickKit/be-sdk-go/v3/ids"
	creditv3 "github.com/brickKit/erp-credit/contract/v3/gen/go/be/erp/credit/v3"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// confirmations walks protocol 04's process table: confirmed, processing and recovered, rejected by a participant,
// a lost answer, an expired branch, a cancel while confirming, a service order, a confirmed order cancelled. The
// order's lifecycle (docs/design.md): SO02 (no change while confirming), SO03, SO04, SO05, SO06, SO08, SO09, SO10.
func (fx *fixture) confirmations() {
	t, inv, cred := fx.t, fx.inv, fx.cred

	// order A: goods and a service; amounts by line, totals the sums; confirmed within the budget
	a := fx.order(fx.goods("2"), fx.service())
	assert.Regexp(t, regexp.MustCompile(`^SO-SH-`+fx.year+`-\d{6}$`), a["number"])
	assert.Equal(t, map[string]any{"net": "390", "tax": "40.2", "gross": "430.2"}, a["totals"])
	assert.Equal(t, "goods", at(a, "lines", 0, "kind"))
	assert.Equal(t, "service", at(a, "lines", 1, "kind"))
	assert.Equal(t, "9", at(a, "lines", 1, "tax"))
	key := ids.New().String()
	ans := fx.confirm(a, key)
	require.Equal(t, "confirmed", ans["outcome"], ans)
	procA := fx.process(a, ans["confirmation_id"].(string))
	invA, credA := branchOf(procA, core.Inventory), branchOf(procA, core.Credit)
	assert.Equal(t, "committed", inv.state(invA))
	assert.Equal(t, "committed", cred.state(credA))
	inv.mu.Lock()
	held := inv.content[invA].([]*inventoryv3.HoldLine)
	inv.mu.Unlock()
	require.Len(t, held, 1)
	assert.Equal(t, []string{fx.screw, fx.wh, "24"}, []string{held[0].ProductId, held[0].WarehouseId, held[0].Quantity},
		"goods lines merged by product and warehouse, in base units")
	cred.mu.Lock()
	assert.Equal(t, "430.2 CNY", cred.content[credA], "the credit branch holds the gross amount")
	cred.mu.Unlock()
	assert.Equal(t, "confirmed", at(ans, "order", "state"))
	assert.Equal(t, map[string]any{"quantity": "1.5", "amount": "150"}, at(ans, "order", "lines", 1, "billable"),
		"a service line is billable once confirmed")
	assert.Equal(t, "0", at(ans, "order", "lines", 0, "billable", "quantity"), "goods become billable when delivered")
	again := fx.confirm(a, key)
	assert.Equal(t, ans["confirmation_id"], again["confirmation_id"], "RS08: the same key answers the same process")
	assert.Equal(t, "confirmed", again["outcome"])
	fx.orders["A"], fx.procs["A"] = a, procA

	// RS07, RS09 and the recovery: credit is down, the answer is "processing"; the order refuses changes; once
	// credit is back the work item confirms the order
	cred.set("hold", "down")
	b := fx.order(fx.goods("1"))
	assert.Equal(t, "processing", fx.confirm(b)["outcome"])
	code, refused := fx.alice.do("PATCH", "/api/sales/orders/"+b["id"].(string), map[string]any{"lines": []any{fx.goods("3")}})
	assert.Equal(t, 409, code)
	assert.Equal(t, "STATE_CONFLICT", refused["reason"])
	cred.set("hold", "")
	require.Eventually(t, func() bool { return fx.orderState(b["id"].(string)) == "confirmed" }, 60*time.Second, 300*time.Millisecond)
	fx.orders["B"] = fx.get("/api/sales/orders/" + b["id"].(string))

	// RS02: credit refuses; the inventory branch is released and the order is a draft again with the reason
	cred.set("hold", "reject")
	c := fx.order(fx.goods("1"))
	ansC := fx.confirm(c)
	assert.Equal(t, "rejected", ansC["outcome"], ansC)
	assert.Equal(t, "over_limit", ansC["reason"])
	assert.Equal(t, "draft", at(ansC, "order", "state"))
	assert.Equal(t, "over_limit", at(ansC, "order", "reason"))
	assert.Equal(t, "released", inv.state(branchOf(fx.process(c, ansC["confirmation_id"].(string)), core.Inventory)))
	cred.set("hold", "")

	// RS03: the hold's answer is lost once; asking by the branch ID finds it held, nothing is held twice
	inv.set("hold", "lost")
	d := fx.order(fx.goods("1"))
	ansD := fx.confirm(d)
	require.Equal(t, "confirmed", ansD["outcome"], ansD)
	procD := fx.process(d, ansD["confirmation_id"].(string))
	inv.mu.Lock()
	assert.Equal(t, 1, inv.holds[branchOf(procD, core.Inventory)])
	inv.mu.Unlock()

	// RS05: a branch expired before the confirm: the others are released, the reason is processing_timeout
	cred.set("confirm", "expire")
	e := fx.order(fx.goods("1"))
	ansE := fx.confirm(e)
	assert.Equal(t, "rejected", ansE["outcome"], ansE)
	assert.Equal(t, "processing_timeout", ansE["reason"])
	assert.Equal(t, "released", inv.state(branchOf(fx.process(e, ansE["confirmation_id"].(string)), core.Inventory)))
	cred.set("confirm", "")

	// RS04: cancelled while confirming; credit's tombstone refuses the late hold
	cred.set("hold", "down")
	f := fx.order(fx.goods("1"))
	ansF := fx.confirm(f)
	assert.Equal(t, "processing", ansF["outcome"], ansF)
	code, cancelled := fx.alice.do("POST", "/api/sales/orders/"+f["id"].(string)+"/cancel", nil)
	require.Equal(t, 200, code, cancelled)
	require.Eventually(t, func() bool { return fx.orderState(f["id"].(string)) == "cancelled" }, 30*time.Second, 300*time.Millisecond)
	procF := fx.process(f, ansF["confirmation_id"].(string))
	assert.Equal(t, "cancelled", procF["state"])
	cred.set("hold", "")
	late, _ := cred.Hold(fx.ctx, &creditv3.HoldRequest{BranchId: branchOf(procF, core.Credit)})
	assert.Equal(t, creditv3.BranchState_BRANCH_STATE_CANCELLED, late.GetBranch().GetState())
	assert.Equal(t, "released", inv.state(branchOf(procF, core.Inventory)))

	// RS14: a service order asks no stock
	g := fx.order(fx.service())
	ansG := fx.confirm(g)
	require.Equal(t, "confirmed", ansG["outcome"], ansG)
	procG := fx.process(g, ansG["confirmation_id"].(string))
	assert.Equal(t, "", branchOf(procG, core.Inventory))
	assert.NotEqual(t, "", branchOf(procG, core.Credit))

	// a confirmed order with nothing delivered cancels through a cancel process over the same branches
	code, cancelD := fx.alice.do("POST", "/api/sales/orders/"+d["id"].(string)+"/cancel", nil)
	require.Equal(t, 200, code, cancelD)
	require.Eventually(t, func() bool { return fx.orderState(d["id"].(string)) == "cancelled" }, 30*time.Second, 300*time.Millisecond)
	assert.Equal(t, "released", inv.state(branchOf(procD, core.Inventory)))
	assert.Equal(t, "released", cred.state(branchOf(procD, core.Credit)))
}
