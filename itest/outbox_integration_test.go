//go:build integration

package itest

import (
	"bytes"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	"github.com/santhosh-tekuri/jsonschema/v6"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

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
	for _, typ := range []string{core.QuotationChanged, core.OrderChanged, core.ConfirmationEnded, core.DeliveryChanged, core.DeliveryPosted,
		core.BillableChanged, core.PriceListChanged, core.PriceListItemChange, core.QuoteRequestRejected} {
		assert.NotZero(t, counts[typ], typ)
	}
}
