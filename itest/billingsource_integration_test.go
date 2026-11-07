//go:build integration

package itest

import (
	"sync"
	"time"

	"github.com/brickKit/be-sdk-go/v3/ids"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// billingSource calls the billing source as erp/billing: BL01, BL02, BL24, BL05/BL26, two claims racing for one
// line, formalizing completes the order and releases its branches; only erp/billing may call it. SO13 (a confirmed
// order is claimed), SO14 (every line delivered and billed completes the order).
func (fx *fixture) billingSource() {
	t, ctx := fx.t, fx.ctx
	a, b := fx.orders["A"], fx.orders["B"]
	conn, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, core.Billing).ClientConfig(core.ID))))
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
	credA := branchOf(fx.procs["A"], core.Credit)
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
	other, err := grpc.NewClient(itSystem, grpc.WithTransportCredentials(credentials.NewTLS(fx.w.TLS(t, core.Inventory).ClientConfig(core.ID))))
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
