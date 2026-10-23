package sales

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/mergesafety"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheManifestDeclaresWhatTheSDKReads: component.yaml's configSchema is exactly the keys the SDK reads.
func TestTheManifestDeclaresWhatTheSDKReads(t *testing.T) {
	raw, err := os.ReadFile("../component.yaml")
	require.NoError(t, err)
	if p := launcher.ManifestProblems(raw, Definition(), nil); len(p) > 0 {
		t.Errorf("component.yaml's configSchema differs from what the SDK reads:\n%v\nthe block:\n%s", p,
			launcher.ConfigSchemaYAML(Definition(), nil))
	}
}

// TestTheManifestListsThePublishedEvents: component.yaml's events.publishes is exactly the event contracts.
func TestTheManifestListsThePublishedEvents(t *testing.T) {
	raw, err := os.ReadFile("../component.yaml")
	require.NoError(t, err)
	assert.Empty(t, launcher.EventProblems(raw, os.DirFS("../contract/events")))
}

// TestTheRoutesAreTheOpenAPIDocument: every route is in contract/openapi.yaml with its operation, and nothing else is.
func TestTheRoutesAreTheOpenAPIDocument(t *testing.T) {
	raw, err := os.ReadFile("../contract/openapi.yaml")
	require.NoError(t, err)
	var r httpx.Routes
	require.NoError(t, (&Module{}).Routes(&r))
	assert.Empty(t, httpx.OpenAPIProblems(raw, r.List()))
}

// TestThePayloadsFitTheirSchemas: what the full-state events and the order read carry is what contract/events
// promises (the facts are checked on the outbox in the integration test).
func TestThePayloadsFitTheirSchemas(t *testing.T) {
	le, pr, wh := "01a15ab0-0000-7000-8000-0000000000a1", "01a15ab0-0000-7000-8000-0000000000c1", "01a15ab0-0000-7000-8000-0000000000b1"
	owner, dept := "01a15ab0-0000-7000-8000-0000000000e1", "01a15ab0-0000-7000-8000-0000000000e2"
	at, item, tax := "2026-10-20T08:00:00.000000Z", "01a15ab0-0000-7000-8000-0000000000f1", "91310000MA1"
	reason, batch, until := "insufficient_stock", "B2026-10", "2026-11-30"
	line := QuoteLine{ID: "01a15ab0-0000-7000-8000-0000000000d1", LineNo: 1, Kind: "goods",
		Product:  ProductSnap{ID: pr, Code: "P-1", Name: "螺丝", BaseUnit: "EA"},
		Quantity: "2", Unit: "BX", ConversionFactor: "12", BaseQuantity: "24", UnitPrice: "120", PriceSource: "price_list",
		PriceListItemID: &item, TaxRate: "0.13", TaxSource: "manual", Net: "240", Tax: "31.2"}
	cust := Customer{PartnerID: "01a15ab0-0000-7000-8000-0000000000c9", Name: "上海客户", TaxID: &tax}
	totals := Totals{Net: "240", Tax: "31.2", Gross: "271.2"}
	for typ, v := range map[string]any{
		QuotationChanged: Quotation{ID: "01a15ab0-0000-7000-8000-0000000000aa", Number: "QT-SH-2026-000001",
			LegalEntityID: le, Customer: cust, Currency: "CNY", OwnerID: owner, DepartmentID: &dept, State: "sent",
			ValidUntil: &until, Lines: []QuoteLine{line}, Totals: totals, Version: 2, CreatedAt: at, UpdatedAt: at},
		OrderChanged: Order{ID: "01a15ab0-0000-7000-8000-0000000000ab", Number: "SO-SH-2026-000001", LegalEntityID: le,
			Customer: cust, Currency: "USD", OwnerID: owner, State: "draft", BusinessDate: "2026-10-20",
			ExchangeRate: &Rate{Rate: "7.1", Date: "2026-10-20", Type: "spot", RecordID: item, RecordVersion: 1},
			Reason:       &reason, Lines: []OrderLine{{QuoteLine: line, WarehouseID: &wh, Delivered: "0",
				Billable: Measure{"0", "0"}, Claimed: Measure{"0", "0"}, Billed: Measure{"0", "0"}}},
			Totals: totals, Version: 3, CreatedAt: at, UpdatedAt: at},
		DeliveryChanged: Delivery{ID: "01a15ab0-0000-7000-8000-0000000000ac", Number: "DN-SH-2026-000001",
			OrderID: "01a15ab0-0000-7000-8000-0000000000ab", LegalEntityID: le, State: "posted", BusinessDate: "2026-10-21",
			Lines: []DeliveryLine{{OrderLineID: line.ID, ProductID: pr, WarehouseID: wh, Quantity: "1", BaseQuantity: "12",
				Batch: &batch, Serials: []string{}}}, OwnerID: owner, Version: 2, CreatedAt: at, UpdatedAt: at},
		PriceListChanged: PriceList{ID: item, LegalEntityID: le, Currency: "CNY", Code: "STD", Name: "标准价", State: "active",
			Version: 1, UpdatedAt: at},
		PriceListItemChange: PriceListItem{ID: item, PriceListID: item, ProductID: pr, Unit: "EA", Price: "10.5",
			ValidFrom: "2026-10-01", ValidTo: &until, Version: 1, UpdatedAt: at},
	} {
		validate(t, typ, v)
	}
}

func validate(t *testing.T, typ string, v any) {
	t.Helper()
	s, err := jsonschema.NewCompiler().Compile("../contract/events/" + typ + ".schema.json")
	require.NoError(t, err, typ)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	require.NoError(t, err)
	assert.NoError(t, s.Validate(doc), typ)
}

// TestTheModuleIsMergeSafe: the module is compiled into the Go core shell.
func TestTheModuleIsMergeSafe(t *testing.T) {
	findings, err := mergesafety.CheckDir(".")
	require.NoError(t, err)
	for _, f := range findings {
		t.Errorf("%v", f)
	}
}
