package sales

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/brickKit/be-sdk-go/v3/audit"
	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/grpcx"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/identity"
	"github.com/brickKit/be-sdk-go/v3/money"
	"github.com/brickKit/be-sdk-go/v3/runtime"
	"github.com/brickKit/be-sdk-go/v3/work"
	salesv3 "github.com/brickKit/erp-sales/contract/v3/gen/go/be/erp/sales/v3"
)

// The component's own reasons (the SDK's catalog has the shared ones).
var (
	errVersion = errs.Define(ID, "VERSION_CONFLICT", errs.Aborted, 409, errs.RetryNo, "The record changed since that version")
	errState   = errs.Define(ID, "STATE_CONFLICT", errs.FailedPrecondition, 409, errs.RetryNo,
		"The record's state does not allow this")
	errLegalEntity = errs.Define(ID, "LEGAL_ENTITY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The legal entity does not exist or is not active")
	errIneligible = errs.Define(ID, "NOT_ELIGIBLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"A customer or product may not be used for a sale in this legal entity now")
	errDepartment = errs.Define(ID, "DEPARTMENT_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The department does not exist, is retired or belongs to another legal entity")
	errCurrency = errs.Define(ID, "CURRENCY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The currency does not exist or is not active")
	errNoRate = errs.Define(ID, "NO_EXCHANGE_RATE", errs.FailedPrecondition, 409, errs.RetryNo,
		"No spot rate to the legal entity's base currency on the business date")
	errNoPrice = errs.Define(ID, "NO_PRICE", errs.FailedPrecondition, 409, errs.RetryNo,
		"No price for the product: enter the unit price")
	errUnit = errs.Define(ID, "NO_CONVERSION", errs.FailedPrecondition, 409, errs.RetryNo,
		"The unit does not convert to the product's base unit")
	errCode = errs.Define(ID, "CODE_TAKEN", errs.AlreadyExists, 409, errs.RetryNo, "The code is taken")
	// the billing source (protocol 05)
	errContent = errs.Define(ID, "CONTENT_CONFLICT", errs.AlreadyExists, 409, errs.RetryNo,
		"The ID was used before with other content")
	errClaim = errs.Define(ID, "CLAIM_STATE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The claim's state does not allow this")
)

var (
	uuidForm     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	currencyForm = regexp.MustCompile(`^[A-Z]{3}$`)
	dateForm     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	codeForm     = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,31}$`)
)

// Module is the sales component.
type Module struct {
	salesv3.UnimplementedBillingSourceServiceServer
	salesv3.UnimplementedOrderServiceServer
	rt *runtime.Runtime
}

// New builds the module from its runtime.
func New(rt *runtime.Runtime) (*Module, error) { return &Module{rt: rt}, nil }

// Routes registers the user surface (contract/openapi.yaml).
func (m *Module) Routes(r *httpx.Routes) error {
	add := func(method, path, op string, h httpx.Handler) {
		r.Add(httpx.Route{Method: method, Path: path, Operation: op, Handle: h})
	}
	add("GET", "/api/sales/quotations", OpQuoteRead, m.listQuotations)
	add("POST", "/api/sales/quotations", OpQuoteEdit, m.createQuotation)
	add("GET", "/api/sales/quotations/:id", OpQuoteRead, m.getQuotation)
	add("PATCH", "/api/sales/quotations/:id", OpQuoteEdit, m.updateQuotation)
	add("POST", "/api/sales/quotations/:id/send", OpQuoteEdit, m.quotationAction("send"))
	add("POST", "/api/sales/quotations/:id/cancel", OpQuoteEdit, m.quotationAction("cancel"))
	add("POST", "/api/sales/quotations/:id/accept", OpQuoteEdit, m.acceptQuotation)
	add("GET", "/api/sales/orders", OpOrderRead, m.listOrders)
	add("POST", "/api/sales/orders", OpOrderEdit, m.createOrder)
	add("GET", "/api/sales/orders/:id", OpOrderRead, m.getOrder)
	add("PATCH", "/api/sales/orders/:id", OpOrderEdit, m.updateOrder)
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/orders/:id/confirm", Operation: OpOrderConfirm,
		Orchestration: true, Handle: m.confirmOrder})
	add("GET", "/api/sales/orders/:id/confirmations/:confirmation_id", OpOrderRead, m.getConfirmation)
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/orders/:id/cancel", Operation: OpOrderCancel,
		Orchestration: true, Handle: m.cancelOrder})
	add("GET", "/api/sales/deliveries", OpDeliveryRead, m.listDeliveries)
	r.Add(httpx.Route{Method: "POST", Path: "/api/sales/deliveries", Operation: OpDeliveryCreate,
		Orchestration: true, Handle: m.createDelivery})
	add("GET", "/api/sales/deliveries/:id", OpDeliveryRead, m.getDelivery)
	add("GET", "/api/sales/price-lists", OpPriceRead, m.listPriceLists)
	add("POST", "/api/sales/price-lists", OpPriceManage, m.createPriceList)
	add("GET", "/api/sales/price-lists/:id", OpPriceRead, m.getPriceList)
	add("PATCH", "/api/sales/price-lists/:id", OpPriceManage, m.updatePriceList)
	add("GET", "/api/sales/price-lists/:id/items", OpPriceRead, m.listPrices)
	add("POST", "/api/sales/price-lists/:id/items", OpPriceManage, m.setPrice)
	// in the contract, not in this version (D75)
	add("POST", "/api/sales/orders/:id/close", OpOrderCancel, m.unsupportedOnOrder("closing what is not delivered"))
	add("POST", "/api/sales/orders/:id/revisions", OpOrderEdit, m.unsupportedOnOrder("revisions"))
	add("POST", "/api/sales/returns", OpReturnEdit, func(c *httpx.Context) error {
		return errs.New(errs.ReasonCapabilityUnsupported, "returns, allowances and exchanges are not in this version of erp/sales")
	})
	return nil
}

func (m *Module) unsupportedOnOrder(what string) httpx.Handler {
	return func(c *httpx.Context) error {
		o, err := m.order(c.Ctx(), c.Param("id"))
		if err != nil {
			return err
		}
		if _, _, err := check(c, c.Operation(), docFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID)); err != nil {
			return err
		}
		return errs.New(errs.ReasonCapabilityUnsupported, what+" is not in this version of erp/sales")
	}
}

// Services registers the system surface: the billing source for erp/billing, the order read for any component.
func (m *Module) Services(s *grpcx.Services) error {
	billing := grpcx.Method{Operation: OpSysBilling, Callers: []string{Billing}}
	s.Register(&salesv3.BillingSourceService_ServiceDesc, m, map[string]grpcx.Method{
		"Ledger": billing, "Claim": billing, "Renew": billing, "Confirm": billing, "Formalize": billing,
		"Release": billing, "Reverse": billing})
	s.Register(&salesv3.OrderService_ServiceDesc, m, map[string]grpcx.Method{
		"Read": {Operation: OpSysRead, Callers: []string{grpcx.AnyComponent}}})
	return nil
}

// Subscriptions declares the department projection's input (protocol 02).
func (m *Module) Subscriptions(s *events.Subscriptions) error {
	s.Add(events.Sub{Name: "org-units", Streams: []string{OrgStream}, Types: []string{OrgUnitChanged}, Handle: m.onOrgUnit})
	return nil
}

// Jobs declares the background work (protocol 01).
func (m *Module) Jobs(j *work.Jobs) error {
	j.Add(work.Job{Name: jobAdvance, Kind: work.Queue, Effect: work.IdempotentExternal, Handle: m.advanceJob,
		DeadLetter: m.advanceDead})
	// an issue is retried until the inventory answers: only it knows whether the goods left
	j.Add(work.Job{Name: jobIssue, Kind: work.Queue, Effect: work.IdempotentExternal, MaxAttempts: work.Unlimited,
		Handle: m.issueJob})
	j.Add(work.Job{Name: "expire-claims", Kind: work.Singleton, Effect: work.Local, Interval: time.Minute,
		MaxAttempts: work.Unlimited, Handle: m.expireClaims})
	j.Add(work.Job{Name: "align-org-units", Kind: work.Singleton, Effect: work.Local, Interval: time.Hour,
		MaxAttempts: work.Unlimited, Handle: m.alignOrgUnits})
	return nil
}

// docFacts are the facts of one document's three dimensions; a document without a department has none (security
// 07: no department never matches a tree, it never widens to the whole organization).
func docFacts(legalEntity string, departmentPath *string, owner string) sdkauthz.Facts {
	dept := sdkauthz.Null
	if departmentPath != nil {
		dept = sdkauthz.Value(*departmentPath)
	}
	return sdkauthz.Facts{DimLegalEntity: sdkauthz.Value(legalEntity), DimDepartment: dept, DimOwner: sdkauthz.Value(owner)}
}

// check decides op on a record with the given facts and returns the context to work in and the acting principal.
func check(c *httpx.Context, op string, facts sdkauthz.Facts) (context.Context, string, error) {
	s, _ := sdkauthz.From(c.Ctx())
	ctx, _, err := s.Check(c.Ctx(), op, facts)
	if err != nil {
		return ctx, "", err
	}
	u, _ := identity.UserFrom(ctx)
	return ctx, u.Subject, nil
}

func invalid(field, why string) error {
	return errs.New(errs.ReasonRequestInvalid, why).WithViolations(errs.Violation{Field: field, Reason: "INVALID", Description: why})
}

func notFound(what string) error { return errs.New(errs.ReasonNotFound, what+" not found") }

// decimal checks a non-negative decimal string of at most scale places and returns its canonical form.
func decimal(field, v string, scale int32, positive bool) (string, error) {
	d, err := money.Parse(v)
	if err != nil || d.Sign() < 0 || positive && d.Sign() == 0 || -d.Exponent > scale {
		return "", invalid(field, field+" is a decimal string with at most "+strconv.Itoa(int(scale))+" places")
	}
	d.Reduce(d)
	return d.Text('f'), nil
}

// change records the business change with the record's new state (protocol 03), in the transaction that made it.
func (m *Module) change(ctx context.Context, tx db.Tx, actor, op, typ, id string, after any) error {
	_, err := m.rt.Audit.Write(ctx, tx, audit.Record{Category: audit.BusinessChange, Operation: op, Outcome: audit.Succeeded,
		Actor: actor, Target: map[string]any{"type": typ, "id": id}, Changes: map[string]any{"after": after}})
	return err
}

// emit writes an event into the outbox, in the transaction that changed the record (protocol 02, S2).
func (m *Module) emit(ctx context.Context, tx db.Tx, typ, id string, version int64, legalEntity string, data any) error {
	_, err := m.rt.Events.Emit(ctx, tx, Stream, events.Event{Type: typ, RecordID: id, Version: version,
		Projection: events.Full, LegalEntity: legalEntity, Data: data})
	return err
}
