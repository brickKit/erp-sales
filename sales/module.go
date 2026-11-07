package sales

import (
	"context"
	"time"

	billingsource "github.com/brickKit/erp-sales/v3/internal/billingsource"
	pricing "github.com/brickKit/erp-sales/v3/internal/pricing"

	confirmations "github.com/brickKit/erp-sales/v3/internal/confirmations"
	core "github.com/brickKit/erp-sales/v3/internal/core"
	deliveries "github.com/brickKit/erp-sales/v3/internal/deliveries"
	orders "github.com/brickKit/erp-sales/v3/internal/orders"
	orgunits "github.com/brickKit/erp-sales/v3/internal/orgunits"
	quotations "github.com/brickKit/erp-sales/v3/internal/quotations"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/work"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
)

// Module is the sales component as the launcher and the shells run it: it only assembles the business modules under
// internal/ (their map is internal/AGENTS.md); the gRPC services answer through them (services.go).
type Module struct {
	salesv3.UnimplementedBillingSourceServiceServer
	salesv3.UnimplementedOrderServiceServer
	b *core.Base
}

// New builds the module from its runtime.
func New(rt *runtime.Runtime) (*Module, error) {
	m := &Module{b: &core.Base{RT: rt}}
	if rt.Tiering != nil { // the orders' history, archived ones included (assembly protocol 11)
		if err := rt.Tiering.Register(orders.OrderClass, orders.Reader(m.b)); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Routes registers the user surface (contract/openapi.yaml).
func (m *Module) Routes(r *httpx.Routes) error {
	add := func(method, path, op string, h httpx.Handler) {
		r.Add(httpx.Route{Method: method, Path: path, Operation: op, Handle: h})
	}
	h := m.handler
	add("GET", "/api/sales/quotations", core.OpQuoteRead, h(quotations.ListQuotations))
	add("POST", "/api/sales/quotations", core.OpQuoteEdit, h(quotations.CreateQuotation))
	add("GET", "/api/sales/quotations/:id", core.OpQuoteRead, h(quotations.GetQuotation))
	add("PATCH", "/api/sales/quotations/:id", core.OpQuoteEdit, h(quotations.UpdateQuotation))
	add("POST", "/api/sales/quotations/:id/send", core.OpQuoteEdit, quotations.QuotationAction(m.b, "send"))
	add("POST", "/api/sales/quotations/:id/cancel", core.OpQuoteEdit, quotations.QuotationAction(m.b, "cancel"))
	add("POST", "/api/sales/quotations/:id/accept", core.OpQuoteEdit, h(quotations.AcceptQuotation))
	add("GET", "/api/sales/orders", core.OpOrderRead, h(orders.ListOrders))
	add("POST", "/api/sales/orders", core.OpOrderEdit, h(orders.CreateOrder))
	add("GET", "/api/sales/orders/:id", core.OpOrderRead, h(orders.GetOrder))
	add("PATCH", "/api/sales/orders/:id", core.OpOrderEdit, h(orders.UpdateOrder))
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/orders/:id/confirm", Operation: core.OpOrderConfirm,
		Orchestration: true, Handle: h(confirmations.ConfirmOrder)})
	add("GET", "/api/sales/orders/:id/confirmations/:confirmation_id", core.OpOrderRead, h(confirmations.GetConfirmation))
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/orders/:id/cancel", Operation: core.OpOrderCancel,
		Orchestration: true, Handle: h(confirmations.CancelOrder)})
	add("GET", "/api/sales/deliveries", core.OpDeliveryRead, h(deliveries.ListDeliveries))
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/deliveries", Operation: core.OpDeliveryCreate,
		Orchestration: true, Handle: h(deliveries.CreateDelivery)})
	add("GET", "/api/sales/deliveries/:id", core.OpDeliveryRead, h(deliveries.GetDelivery))
	add("GET", "/api/sales/price-lists", core.OpPriceRead, h(pricing.ListPriceLists))
	add("POST", "/api/sales/price-lists", core.OpPriceManage, h(pricing.CreatePriceList))
	add("GET", "/api/sales/price-lists/:id", core.OpPriceRead, h(pricing.GetPriceList))
	add("PATCH", "/api/sales/price-lists/:id", core.OpPriceManage, h(pricing.UpdatePriceList))
	add("GET", "/api/sales/price-lists/:id/items", core.OpPriceRead, h(pricing.ListPrices))
	add("POST", "/api/sales/price-lists/:id/items", core.OpPriceManage, h(pricing.SetPrice))
	// in the contract, not in this version (D75)
	add("POST", "/api/sales/orders/:id/close", core.OpOrderCancel, orders.UnsupportedOnOrder(m.b, "closing what is not delivered"))
	add("POST", "/api/sales/orders/:id/revisions", core.OpOrderEdit, orders.UnsupportedOnOrder(m.b, "revisions"))
	add("POST", "/api/sales/returns", core.OpReturnEdit, func(c *httpx.Context) error {
		return errs.New(errs.ReasonCapabilityUnsupported, "returns, allowances and exchanges are not in this version of erp/sales")
	})
	return nil
}

// Services registers the system surface: the billing source for erp/billing, the order read for any component.
func (m *Module) Services(s *grpcx.Services) error {
	billing := grpcx.Method{Operation: core.OpSysBilling, Callers: []string{core.Billing}}
	s.Register(&salesv3.BillingSourceService_ServiceDesc, m, map[string]grpcx.Method{
		"Ledger": billing, "Claim": billing, "Renew": billing, "Confirm": billing, "Formalize": billing,
		"Release": billing, "Reverse": billing})
	s.Register(&salesv3.OrderService_ServiceDesc, m, map[string]grpcx.Method{
		"Read": {Operation: core.OpSysRead, Callers: []string{grpcx.AnyComponent}}})
	return nil
}

// Subscriptions declares the department projection's input (protocol 02). The subscription took every department
// event under a new name in 3.0.16: an existing durable subscription keeps its settings (protocol P6.18), so the old
// name would have kept the old type filter, and a version still running during an upgrade keeps its own.
func (m *Module) Subscriptions(s *events.Subscriptions) error {
	s.Add(events.Sub{Name: "org-unit-events", Streams: []string{core.OrgStream}, Types: []string{core.OrgUnitEvents},
		Handle: m.received(orgunits.OnOrgUnit)})
	return nil
}

// Jobs declares the background work (protocol 01).
func (m *Module) Jobs(j *work.Jobs) error {
	j.Add(work.Job{Name: confirmations.JobAdvance, Kind: work.Queue, Effect: work.IdempotentExternal, Handle: m.job(confirmations.AdvanceJob),
		DeadLetter: m.dead(confirmations.AdvanceDead)})
	// an issue is retried until the inventory answers: only it knows whether the goods left
	j.Add(work.Job{Name: deliveries.JobIssue, Kind: work.Queue, Effect: work.IdempotentExternal, MaxAttempts: work.Unlimited,
		Handle: m.job(deliveries.IssueJob)})
	j.Add(work.Job{Name: "expire-claims", Kind: work.Singleton, Effect: work.Local, Interval: time.Minute,
		MaxAttempts: work.Unlimited, Handle: m.job(billingsource.ExpireClaims)})
	j.Add(work.Job{Name: "align-org-units", Kind: work.Singleton, Effect: work.Local, Interval: time.Hour,
		MaxAttempts: work.Unlimited, Handle: m.job(orgunits.AlignOrgUnits)})
	j.Add(work.Job{Name: orgunits.JobRepairOrgUnits, Kind: work.Queue, Effect: work.IdempotentExternal,
		MaxAttempts: work.Unlimited, Handle: m.job(orgunits.RepairOrgUnits)})
	j.Add(work.Job{Name: orgunits.JobRealignOrgUnits, Kind: work.Queue, Effect: work.IdempotentExternal,
		MaxAttempts: work.Unlimited, Handle: m.job(orgunits.RealignOrgUnits)})
	return nil
}

// handler, job, dead and received adapt a module's function, which takes the shared base first, to what the SDK
// registers.
func (m *Module) handler(f func(*core.Base, *httpx.Context) error) httpx.Handler {
	return func(c *httpx.Context) error { return f(m.b, c) }
}

func (m *Module) job(f func(*core.Base, context.Context, *work.Run) error) func(context.Context, *work.Run) error {
	return func(ctx context.Context, r *work.Run) error { return f(m.b, ctx, r) }
}

func (m *Module) dead(f func(*core.Base, context.Context, db.Tx, work.Item, error) error) func(context.Context, db.Tx, work.Item, error) error {
	return func(ctx context.Context, tx db.Tx, item work.Item, err error) error {
		return f(m.b, ctx, tx, item, err)
	}
}

func (m *Module) received(f func(*core.Base, context.Context, db.Tx, events.Received) error) func(context.Context, db.Tx, events.Received) error {
	return func(ctx context.Context, tx db.Tx, e events.Received) error { return f(m.b, ctx, tx, e) }
}
