//go:build integration

package itest

import (
	"regexp"

	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
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
