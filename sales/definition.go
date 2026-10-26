// Package sales is the sales component: quotations, orders and their confirmation (the coordinator of protocol 04),
// deliveries through the inventory, price lists, and the billable ledger erp/billing claims from (protocol 05).
package sales

import (
	"embed"
	"io/fs"
	"time"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/component"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/runtime"
)

// Identity.
const (
	ID       = "erp/sales"
	Version  = "3.0.8"
	GRPCPort = 9094
)

// Dependencies (the IDs runtime.GRPC takes). Inventory and credit are optional participants.
const (
	Partner   = "mdm/partner"
	Product   = "mdm/product"
	Org       = "mdm/org"
	Currency  = "mdm/currency"
	Inventory = "erp/inventory"
	Credit    = "erp/credit"
	// Billing is the user of the billable ledger (protocol 05).
	Billing = "erp/billing"
)

// The stream and its event types (contract/events).
const (
	Stream              = "erp.sales"
	QuotationChanged    = "erp.quotation.changed.v1"
	OrderChanged        = "erp.sales_order.changed.v1"
	ConfirmationEnded   = "erp.sales_order.confirmation_ended.v1"
	DeliveryChanged     = "erp.delivery.changed.v1"
	DeliveryPosted      = "erp.delivery.posted.v1"
	BillableChanged     = "erp.billable.changed.v1"
	PriceListChanged    = "erp.price_list.changed.v1"
	PriceListItemChange = "erp.price_list_item.changed.v1"
	// the source stream of the department projection
	OrgStream       = "mdm.org"
	OrgUnitChanged  = "mdm.org_unit.changed.v1"
	OrgUnitsProject = "org_units"
)

// Resources and operations (contract/openapi.yaml names each route's operation).
const (
	Quotations       = "erp.sales.quotation"
	OpQuoteRead      = "erp.sales.quotation.read"
	OpQuoteEdit      = "erp.sales.quotation.edit"
	Orders           = "erp.sales.order"
	OpOrderRead      = "erp.sales.order.read"
	OpOrderEdit      = "erp.sales.order.edit"
	OpOrderConfirm   = "erp.sales.order.confirm"
	OpOrderCancel    = "erp.sales.order.cancel"
	Deliveries       = "erp.sales.delivery"
	OpDeliveryRead   = "erp.sales.delivery.read"
	OpDeliveryCreate = "erp.sales.delivery.create"
	PriceLists       = "erp.sales.price_list"
	OpPriceRead      = "erp.sales.price_list.read"
	OpPriceManage    = "erp.sales.price_list.manage"
	Returns          = "erp.sales.return"
	OpReturnRead     = "erp.sales.return.read"
	OpReturnEdit     = "erp.sales.return.edit"
	System           = "erp.sales.system"
	OpSysRead        = "erp.sales.system.read"
	OpSysBilling     = "erp.sales.system.billing"
)

// Dimensions (security 07): the legal entity, the department (an org-tree path) and the owner.
const (
	DimLegalEntity = "legal_entity"
	DimDepartment  = "department"
	DimOwner       = "owner"
)

// Parameters (protocol 04, protocol 05, protocol 07).
const (
	SyncBudget    = 3 * time.Second    // the confirmation's synchronous budget
	EvidenceW     = 30 * time.Second   // how long an eligibility answer is evidence
	ClaimPeriod   = 7 * 24 * time.Hour // a pending billing claim's period
	MaxLines      = 500
	MaxReadIDs    = 500
	PriceScale    = 6 // unit prices: NUMERIC(19,6)
	QuantityScale = 6
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

func fields(names ...string) map[string]sdkauthz.Field {
	out := map[string]sdkauthz.Field{}
	for _, n := range names {
		out[n] = sdkauthz.Field{Class: sdkauthz.Ordinary}
	}
	return out
}

// scoped are the three dimensions of quotations, orders and deliveries.
func scoped() map[string]sdkauthz.Dimension {
	return map[string]sdkauthz.Dimension{
		DimLegalEntity: {Kind: sdkauthz.ValueSet, Column: "legal_entity_id"},
		DimDepartment:  {Kind: sdkauthz.OrgTree, Column: "department_path"},
		DimOwner:       {Kind: sdkauthz.Principal, Column: "owner_id", PrincipalRole: "owner"},
	}
}

var documentFields = []string{"id", "number", "legal_entity_id", "customer", "currency", "owner_id", "department_id",
	"state", "lines", "totals", "version", "created_at", "updated_at", "business_date", "exchange_rate", "revision",
	"quotation_id", "confirmation_id", "reason", "valid_until", "order_id", "opportunity_ref", "to_deliver", "to_bill"}

// Definitions are the component's resources and operations.
func Definitions() *sdkauthz.Definitions {
	low := func(key, typ, purpose string, audit bool) sdkauthz.Operation {
		op := sdkauthz.Operation{Key: key, ResourceType: typ, Purpose: purpose, RiskClass: "low", Audit: audit}
		if purpose != "read" {
			op.SideEffects = []string{"business_write"}
		}
		return op
	}
	return &sdkauthz.Definitions{Component: ID,
		Resources: []sdkauthz.Resource{
			{Type: Quotations, ViewOperation: OpQuoteRead, Dimensions: scoped(), Fields: fields(documentFields...)},
			{Type: Orders, ViewOperation: OpOrderRead, Dimensions: scoped(), Fields: fields(documentFields...)},
			{Type: Deliveries, ViewOperation: OpDeliveryRead, Dimensions: scoped(),
				Fields: fields("id", "number", "order_id", "legal_entity_id", "state", "reason", "business_date", "lines",
					"owner_id", "department_id", "version", "created_at", "updated_at")},
			{Type: PriceLists, ViewOperation: OpPriceRead,
				Dimensions: map[string]sdkauthz.Dimension{DimLegalEntity: {Kind: sdkauthz.ValueSet, Column: "legal_entity_id"}},
				Fields: fields("id", "legal_entity_id", "currency", "code", "name", "state", "version", "updated_at",
					"price_list_id", "product_id", "unit", "price", "valid_from", "valid_to")},
			{Type: Returns, ViewOperation: OpReturnRead, Dimensions: scoped(), Fields: fields("id")},
			{Type: System, ViewOperation: OpSysRead, Dimensions: map[string]sdkauthz.Dimension{},
				Fields: map[string]sdkauthz.Field{}},
		},
		Operations: []sdkauthz.Operation{
			low(OpQuoteRead, Quotations, "read", false),
			low(OpQuoteEdit, Quotations, "manage", true),
			low(OpOrderRead, Orders, "read", false),
			low(OpOrderEdit, Orders, "manage", true),
			low(OpOrderConfirm, Orders, "action", true),
			low(OpOrderCancel, Orders, "action", true),
			low(OpDeliveryRead, Deliveries, "read", false),
			low(OpDeliveryCreate, Deliveries, "create", true),
			low(OpPriceRead, PriceLists, "read", false),
			low(OpPriceManage, PriceLists, "manage", true),
			low(OpReturnRead, Returns, "read", false),
			low(OpReturnEdit, Returns, "manage", true),
			low(OpSysRead, System, "read", false),
			{Key: OpSysBilling, ResourceType: System, Purpose: "action", RiskClass: "low", SideEffects: []string{"business_write"}},
		}}
}

// Definition is the component as the launcher and the shells take it.
func Definition() component.Definition {
	return component.Definition{ID: ID, Version: Version, Migrations: migrations(), GRPCPort: GRPCPort, Subscribes: true,
		Streams: []events.Stream{{Name: Stream, Partitions: 4}}, Authz: Definitions(),
		New: func(rt *runtime.Runtime) (component.Module, error) { return New(rt) }}
}
