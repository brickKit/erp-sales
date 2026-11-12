//go:build integration

package itest

import (
	"encoding/json"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/sectest"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

var opportunityStream = events.Stream{Name: core.OpportunityStream, Partitions: 1}

// quoteRequests: crm/opportunity's quote request makes one quotation draft, however often it is delivered (QR01); the
// draft carries the opportunity, the request and the requested products, has no lines and is not sent until a user
// adds them (QR03, QR05); accepting it carries the opportunity to the order (QR05); a request for a customer without
// eligibility makes no draft and publishes the refusal (QR04).
func (fx *fixture) quoteRequests() {
	t, alice := fx.t, fx.alice
	opp, req, other := ids.New().String(), ids.New().String(), ids.New().String()
	fx.publishQuoteRequest(req, opp, fx.cust)
	fx.publishQuoteRequest(req, opp, fx.cust) // delivered twice (a resend, another event ID)

	var q map[string]any
	require.Eventually(t, func() bool {
		q = fx.quotationOf(req)
		return q != nil
	}, 15*time.Second, 100*time.Millisecond, "the quote request makes a draft")
	assert.Equal(t, "draft", q["state"])
	assert.Equal(t, opp, q["opportunity_ref"])
	assert.Equal(t, sectest.Alice, q["owner_id"], "the request's owner")
	assert.Nil(t, q["department_id"])
	assert.Empty(t, q["lines"])
	assert.Equal(t, fx.screw, at(q, "requested_products", 0, "product_id"))
	assert.Equal(t, "3", at(q, "requested_products", 0, "quantity"))

	code, sent := alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/send", nil)
	assert.Equal(t, 400, code, "a draft without lines is not sent")
	assert.Equal(t, "REQUEST_INVALID", sent["reason"])
	code, q = alice.do("PATCH", "/api/sales/quotations/"+q["id"].(string), map[string]any{"lines": []any{fx.goods("3")}})
	require.Equal(t, 200, code, q)
	code, q = alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/send", nil)
	require.Equal(t, 200, code, q)
	code, q = alice.do("POST", "/api/sales/quotations/"+q["id"].(string)+"/accept", nil)
	require.Equal(t, 200, code, q)
	o := fx.get("/api/sales/orders/" + q["order_id"].(string))
	assert.Equal(t, opp, o["opportunity_ref"], "the order carries the opportunity")

	// a request for the blocked customer: no draft, the refusal is published (fact ID = the request)
	fx.publishQuoteRequest(other, opp, fx.blocked)
	require.Eventually(t, func() bool {
		var reason string
		err := fx.admin.QueryRow(fx.ctx, "SELECT convert_from(payload, 'UTF8')::jsonb->>'reason' FROM "+itSchema+".be_outbox WHERE type = $1 AND convert_from(payload, 'UTF8')::jsonb->>'fact_id' = $2",
			core.QuoteRequestRejected, other).Scan(&reason)
		return err == nil && reason == "NOT_ELIGIBLE"
	}, 15*time.Second, 100*time.Millisecond, "the refusal is published")
	assert.Nil(t, fx.quotationOf(other))
	assert.NotNil(t, fx.quotationOf(req), "still one draft for the first request")
	var drafts int
	require.NoError(t, fx.admin.QueryRow(fx.ctx, "SELECT count(*) FROM "+itSchema+".quotation WHERE quote_request_id = $1", req).Scan(&drafts))
	assert.Equal(t, 1, drafts, "a request delivered twice makes one draft")
}

// quotationOf is the quotation made for a quote request, or nil.
func (fx *fixture) quotationOf(request string) map[string]any {
	code, page := fx.alice.do("GET", "/api/sales/quotations?limit=200", nil)
	require.Equal(fx.t, 200, code, page)
	for _, q := range page["quotations"].([]any) {
		if m := q.(map[string]any); m["quote_request_id"] == request {
			return m
		}
	}
	return nil
}

// publishQuoteRequest publishes crm.opportunity.quote_requested.v1 as crm/opportunity's forwarder would, each time
// under a new event ID.
func (fx *fixture) publishQuoteRequest(request, opportunity, customer string) {
	data, _ := json.Marshal(map[string]any{"fact_id": request, "opportunity_id": opportunity, "opportunity_number": "OP-SH-" + fx.year + "-000001",
		"opportunity_name": "上海客户的螺丝", "legal_entity_id": fx.le, "customer_id": customer, "currency": "CNY",
		"owner_id": sectest.Alice, "lines": []any{map[string]any{"product_id": fx.screw, "quantity": "3", "unit": "EA"}},
		"requested_at": time.Now().UTC().Format(time.RFC3339)})
	epoch, err := events.ParseEpoch("1.1")
	require.NoError(fx.t, err)
	row := events.Row{EventID: ids.New().String(), Stream: opportunityStream.Name, Partition: 0, Type: core.QuoteRequested,
		OccurredAt: time.Now(), RecordID: request, Version: 1, Projection: events.Full, Payload: data,
		Seq: time.Now().UnixNano(), Epoch: epoch}
	row.Digest = events.Digest(row.EventID, data)
	require.NoError(fx.t, fx.tr.Publish(fx.ctx, events.Message{Subject: events.Subject(opportunityStream.Name, 0, row.Type),
		Headers: row.Headers(core.Opportunity, "opportunity-it"), Data: data}))
}
