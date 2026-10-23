//go:build integration

package sales

// The steps of TestSalesEndToEnd, in the order they run; each leaves its orders in fx.orders for the next.

import (
	"bytes"
	"net/url"
	"regexp"
	"sync"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	creditv3 "github.com/brickKit/erp-credit/contract/v3/gen/go/be/erp/credit/v3"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// pricingAndQuotations: a price per EA, a box takes it times the factor (docs/rules/pricing.md); a quotation is sent
// and accepted into an order draft with the same prices; missing prices and ineligible customers are refused.
func (fx *fixture) pricingAndQuotations() {
	t, alice := fx.t, fx.alice
	code, pl := alice.do("POST", "/api/sales/price-lists", map[string]any{"legal_entity_id": fx.le, "currency": "CNY",
		"code": "STD", "name": "标准价"})
	require.Equal(t, 200, code, pl)
	code, item := alice.do("POST", "/api/sales/price-lists/"+pl["id"].(string)+"/items", map[string]any{
		"product_id": fx.screw, "unit": "EA", "price": "10", "valid_from": "2026-01-01"})
	require.Equal(t, 200, code, item)
	code, dup := alice.do("POST", "/api/sales/price-lists", map[string]any{"legal_entity_id": fx.le, "currency": "CNY",
		"code": "STD", "name": "又一个"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "CODE_TAKEN", dup["reason"])

	code, q := alice.do("POST", "/api/sales/quotations", map[string]any{"legal_entity_id": fx.le, "customer_id": fx.cust,
		"currency": "CNY", "valid_until": "2099-12-31", "lines": []any{fx.goods("2")}})
	require.Equal(t, 200, code, q)
	assert.Regexp(t, regexp.MustCompile(`^QT-SH-`+fx.year+`-\d{6}$`), q["number"])
	assert.Equal(t, "120", at(q, "lines", 0, "unit_price"), "the base unit's price times the factor")
	assert.Equal(t, "price_list", at(q, "lines", 0, "price_source"))
	assert.Equal(t, "24", at(q, "lines", 0, "base_quantity"))
	code, _ = alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/accept", nil)
	assert.Equal(t, 409, code, "a draft quotation is sent before it is accepted")
	code, q = alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/send", nil)
	require.Equal(t, 200, code, q)
	code, q = alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/accept", nil)
	require.Equal(t, 200, code, q)
	assert.Equal(t, "accepted", q["state"])
	fromQ := fx.get("/api/sales/orders/" + q["order_id"].(string))
	assert.Equal(t, "draft", fromQ["state"])
	assert.Equal(t, q["id"], fromQ["quotation_id"])
	assert.Equal(t, "120", at(fromQ, "lines", 0, "unit_price"))
	assert.Equal(t, "271.2", at(fromQ, "totals", "gross"))

	code, noPrice := alice.do("POST", "/api/sales/orders", map[string]any{"legal_entity_id": fx.le, "customer_id": fx.cust,
		"currency": "CNY", "lines": []any{map[string]any{"product_id": fx.bolt, "quantity": "1", "unit": "EA",
			"warehouse_id": fx.wh, "tax_rate": "0.13"}}})
	assert.Equal(t, 409, code)
	assert.Equal(t, "NO_PRICE", noPrice["reason"], "a line without a price needs the price by hand")
	code, notEligible := alice.do("POST", "/api/sales/orders", map[string]any{"legal_entity_id": fx.le,
		"customer_id": fx.blocked, "currency": "CNY", "lines": []any{fx.service()}})
	assert.Equal(t, 409, code)
	assert.Equal(t, "NOT_ELIGIBLE", notEligible["reason"])
}

// confirmations walks protocol 04's process table: confirmed, processing and recovered, rejected by a participant,
// a lost answer, an expired branch, a cancel while confirming, a service order, a confirmed order cancelled.
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
	invA, credA := branchOf(procA, Inventory), branchOf(procA, Credit)
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
	assert.Equal(t, "released", inv.state(branchOf(fx.process(c, ansC["confirmation_id"].(string)), Inventory)))
	cred.set("hold", "")

	// RS03: the hold's answer is lost once; asking by the branch ID finds it held, nothing is held twice
	inv.set("hold", "lost")
	d := fx.order(fx.goods("1"))
	ansD := fx.confirm(d)
	require.Equal(t, "confirmed", ansD["outcome"], ansD)
	procD := fx.process(d, ansD["confirmation_id"].(string))
	inv.mu.Lock()
	assert.Equal(t, 1, inv.holds[branchOf(procD, Inventory)])
	inv.mu.Unlock()

	// RS05: a branch expired before the confirm: the others are released, the reason is processing_timeout
	cred.set("confirm", "expire")
	e := fx.order(fx.goods("1"))
	ansE := fx.confirm(e)
	assert.Equal(t, "rejected", ansE["outcome"], ansE)
	assert.Equal(t, "processing_timeout", ansE["reason"])
	assert.Equal(t, "released", inv.state(branchOf(fx.process(e, ansE["confirmation_id"].(string)), Inventory)))
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
	late, _ := cred.Hold(fx.ctx, &creditv3.HoldRequest{BranchId: branchOf(procF, Credit)})
	assert.Equal(t, creditv3.BranchState_BRANCH_STATE_CANCELLED, late.GetBranch().GetState())
	assert.Equal(t, "released", inv.state(branchOf(procF, Inventory)))

	// RS14: a service order asks no stock
	g := fx.order(fx.service())
	ansG := fx.confirm(g)
	require.Equal(t, "confirmed", ansG["outcome"], ansG)
	procG := fx.process(g, ansG["confirmation_id"].(string))
	assert.Equal(t, "", branchOf(procG, Inventory))
	assert.NotEqual(t, "", branchOf(procG, Credit))

	// a confirmed order with nothing delivered cancels through a cancel process over the same branches
	code, cancelD := fx.alice.do("POST", "/api/sales/orders/"+d["id"].(string)+"/cancel", nil)
	require.Equal(t, 200, code, cancelD)
	require.Eventually(t, func() bool { return fx.orderState(d["id"].(string)) == "cancelled" }, 30*time.Second, 300*time.Millisecond)
	assert.Equal(t, "released", inv.state(branchOf(procD, Inventory)))
	assert.Equal(t, "released", cred.state(branchOf(procD, Credit)))
}

// deliveries: the delivery ID is the consumption ID (RS19); the billable amount grows by rounding and the last
// delivery makes it exactly the line amount; a refused issue fails the delivery; an unanswered one is posted later.
func (fx *fixture) deliveries() {
	t, inv := fx.t, fx.inv
	a, b := fx.orders["A"], fx.orders["B"]
	invA := branchOf(fx.procs["A"], Inventory)
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

// billingSource calls the billing source as erp/billing: BL01, BL02, BL24, BL05/BL26, two claims racing for one
// line, formalizing completes the order and releases its branches; only erp/billing may call it.
func (fx *fixture) billingSource() {
	t, ctx := fx.t, fx.ctx
	a, b := fx.orders["A"], fx.orders["B"]
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, Billing).ClientConfig(ID))))
	require.NoError(t, err)
	defer conn.Close()
	src := salesv3.NewBillingSourceServiceClient(conn)
	lineA, lineS, lineB := at(a, "lines", 0, "id").(string), at(a, "lines", 1, "id").(string), at(b, "lines", 0, "id").(string)
	m := func(q, a string) *salesv3.Measure { return &salesv3.Measure{Quantity: q, Amount: a} }
	all := []*salesv3.ClaimLine{{OrderLineId: lineA, Measure: m("2", "240")}, {OrderLineId: lineS, Measure: m("1.5", "150")}}

	c1 := ids.New().String()
	got, err := src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: c1, OrderId: a["id"].(string), Lines: all})
	require.NoError(t, err)
	require.True(t, got.GetClaimed(), got.GetProblems())
	assert.Equal(t, salesv3.ClaimState_CLAIM_STATE_PENDING, got.GetClaim().GetState())
	got, err = src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: c1, OrderId: a["id"].(string), Lines: all})
	require.NoError(t, err)
	assert.True(t, got.GetClaimed(), "BL02: the same claim again answers the first")
	_, err = src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: c1, OrderId: a["id"].(string), Lines: all[:1]})
	assert.Equal(t, codes.AlreadyExists, status.Code(err), "the same claim ID with other content")
	got, err = src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: ids.New().String(), OrderId: a["id"].(string),
		Lines: []*salesv3.ClaimLine{{OrderLineId: lineS, Measure: m("0.5", "50")}}})
	require.NoError(t, err)
	assert.False(t, got.GetClaimed(), "BL01: nothing is left to claim")
	assert.Equal(t, "exceeds_billable", got.GetProblems()[0].GetReason())
	ledger, err := src.Ledger(ctx, &salesv3.LedgerRequest{OrderId: a["id"].(string)})
	require.NoError(t, err)
	assert.Equal(t, "2", ledger.GetLines()[0].GetClaimed().GetQuantity())
	conf, err := src.Confirm(ctx, &salesv3.ConfirmRequest{ClaimId: c1})
	require.NoError(t, err)
	assert.True(t, conf.GetConfirmed())
	conf, err = src.Confirm(ctx, &salesv3.ConfirmRequest{ClaimId: c1})
	require.NoError(t, err)
	assert.True(t, conf.GetConfirmed(), "BL24: confirming again answers the same")
	form, err := src.Formalize(ctx, &salesv3.FormalizeRequest{ClaimId: c1, Billed: all})
	require.NoError(t, err)
	assert.Equal(t, salesv3.ClaimState_CLAIM_STATE_FORMALIZED, form.GetClaim().GetState())
	assert.Equal(t, "completed", fx.orderState(a["id"].(string)), "every line delivered and billed")
	credA := branchOf(fx.procs["A"], Credit)
	require.Eventually(t, func() bool { return fx.cred.state(credA) == "released" }, 30*time.Second, 300*time.Millisecond,
		"the release process gives the branches back")

	// two drafts claim the same remaining quantity at once: exactly one wins
	var wg sync.WaitGroup
	wins := make([]bool, 2)
	claims := []string{ids.New().String(), ids.New().String()}
	for i := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: claims[i], OrderId: b["id"].(string),
				Lines: []*salesv3.ClaimLine{{OrderLineId: lineB, Measure: m("1", "120")}}})
			wins[i] = err == nil && r.GetClaimed()
		}()
	}
	wg.Wait()
	assert.NotEqual(t, wins[0], wins[1], "exactly one claim takes the line")
	winner := claims[0]
	if wins[1] {
		winner = claims[1]
	}
	// BL05/BL26: a claim past its period is refused at confirm and goes back to the ledger
	_, err = fx.admin.Exec(ctx, "UPDATE "+itSchema+".billing_claim SET expires_at = now() - interval '1 second' WHERE id = $1", winner)
	require.NoError(t, err)
	conf, err = src.Confirm(ctx, &salesv3.ConfirmRequest{ClaimId: winner})
	require.NoError(t, err)
	assert.False(t, conf.GetConfirmed())
	assert.Equal(t, salesv3.ClaimState_CLAIM_STATE_EXPIRED, conf.GetClaim().GetState())
	ledger, err = src.Ledger(ctx, &salesv3.LedgerRequest{OrderId: b["id"].(string)})
	require.NoError(t, err)
	assert.Equal(t, "0", ledger.GetLines()[0].GetClaimed().GetQuantity())
	c3 := ids.New().String()
	got, err = src.Claim(ctx, &salesv3.ClaimRequest{ClaimId: c3, OrderId: b["id"].(string),
		Lines: []*salesv3.ClaimLine{{OrderLineId: lineB, Measure: m("1", "120")}}})
	require.NoError(t, err)
	require.True(t, got.GetClaimed())
	rel, err := src.Release(ctx, &salesv3.ReleaseRequest{ClaimId: c3})
	require.NoError(t, err)
	assert.Equal(t, salesv3.ClaimState_CLAIM_STATE_RELEASED, rel.GetClaim().GetState())
	_, err = src.Reverse(ctx, &salesv3.ReverseRequest{ClaimId: c3})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "corrections are not in this version")

	// only erp/billing calls the billing source; any component reads orders
	other, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, Inventory).ClientConfig(ID))))
	require.NoError(t, err)
	defer other.Close()
	_, err = salesv3.NewBillingSourceServiceClient(other).Ledger(ctx, &salesv3.LedgerRequest{OrderId: b["id"].(string)})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	read, err := salesv3.NewOrderServiceClient(other).Read(ctx, &salesv3.ReadRequest{Ids: []string{a["id"].(string), ids.New().String()}})
	require.NoError(t, err)
	assert.Equal(t, salesv3.ReadState_READ_STATE_EXISTS, read.GetResults()[0].GetState())
	assert.Contains(t, string(read.GetResults()[0].GetData()), `"state":"completed"`)
	assert.Equal(t, salesv3.ReadState_READ_STATE_NOT_FOUND, read.GetResults()[1].GetState())
}

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

// departments: Bob reads the orders of department 1 and below; department 2 moves under it and its orders follow.
func (fx *fixture) departments() {
	t := fx.t
	inD1 := fx.newOrder(map[string]any{"department_id": fx.d1, "lines": []any{fx.service()}})["id"]
	inD2 := fx.newOrder(map[string]any{"department_id": fx.d2, "lines": []any{fx.service()}})["id"]
	fx.w.Grant(sectest.Bob, sdkauthz.Segment{ResourceType: Orders, Operation: OpOrderRead, ReadFields: documentFields,
		Atoms: map[string]sdkauthz.Atom{DimLegalEntity: {Kind: "all"}, DimOwner: {Kind: "all"},
			DimDepartment: {Kind: "tree", Items: []sdkauthz.TreeItem{{Path: fx.le + "/" + fx.d1, Mode: "subtree"}}}},
		Valid: sdkauthz.Window{From: "2026-01-01T00:00:00Z"}})
	bob := client{t, fx.w.Bearer(sectest.Bob)}
	seen := func() map[any]bool {
		_, page := bob.do("GET", "/api/sales/orders", nil)
		out := map[any]bool{}
		listed, _ := page["orders"].([]any)
		for _, o := range listed {
			out[at(o, "id")] = true
		}
		return out
	}
	require.Eventually(t, func() bool { return seen()[inD1] }, 30*time.Second, 200*time.Millisecond)
	assert.False(t, seen()[inD2], "department 2 is outside Bob's tree")
	code, _ := bob.do("GET", "/api/sales/orders/"+inD2.(string), nil)
	assert.Equal(t, 404, code)
	moved := fx.unit(fx.d2, fx.le+"/"+fx.d1+"/"+fx.d2, 2)
	moved["parent_id"] = fx.d1
	fx.org.mu.Lock()
	fx.org.units[fx.d2] = moved
	fx.org.mu.Unlock()
	fx.publishOrgUnit(moved)
	require.Eventually(t, func() bool { return seen()[inD2] }, 30*time.Second, 200*time.Millisecond,
		"the department's move rewrites the copied path")
}

// outbox: every event fits its schema, and every type was published.
func (fx *fixture) outbox() {
	t := fx.t
	rows, err := fx.admin.Query(fx.ctx, "SELECT type, payload FROM "+itSchema+".be_outbox ORDER BY ord")
	require.NoError(t, err)
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var typ string
		var payload []byte
		require.NoError(t, rows.Scan(&typ, &payload))
		counts[typ]++
		s, err := jsonschema.NewCompiler().Compile("../contract/events/" + typ + ".schema.json")
		require.NoError(t, err, typ)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
		require.NoError(t, err)
		assert.NoError(t, s.Validate(doc), "%s: %s", typ, payload)
	}
	for _, typ := range []string{QuotationChanged, OrderChanged, ConfirmationEnded, DeliveryChanged, DeliveryPosted,
		BillableChanged, PriceListChanged, PriceListItemChange} {
		assert.NotZero(t, counts[typ], typ)
	}
}
