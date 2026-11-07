package core

import (
	"context"
	"regexp"
	"strconv"

	"github.com/brickKit/be-sdk-go/v3/audit"
	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/identity"
	"github.com/brickKit/be-sdk-go/v3/money"
	apd "github.com/cockroachdb/apd/v3"
)

// The component's own reasons (the SDK's catalog has the shared ones).
var (
	ErrVersion = errs.Define(ID, "VERSION_CONFLICT", errs.Aborted, 409, errs.RetryNo, "The record changed since that version")
	ErrState   = errs.Define(ID, "STATE_CONFLICT", errs.FailedPrecondition, 409, errs.RetryNo,
		"The record's state does not allow this")
	ErrLegalEntity = errs.Define(ID, "LEGAL_ENTITY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The legal entity does not exist or is not active")
	ErrIneligible = errs.Define(ID, "NOT_ELIGIBLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"A customer or product may not be used for a sale in this legal entity now")
	ErrDepartment = errs.Define(ID, "DEPARTMENT_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The department does not exist, is retired or belongs to another legal entity")
	ErrCurrency = errs.Define(ID, "CURRENCY_NOT_USABLE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The currency does not exist or is not active")
	ErrNoRate = errs.Define(ID, "NO_EXCHANGE_RATE", errs.FailedPrecondition, 409, errs.RetryNo,
		"No spot rate to the legal entity's base currency on the business date")
	ErrNoPrice = errs.Define(ID, "NO_PRICE", errs.FailedPrecondition, 409, errs.RetryNo,
		"No price for the product: enter the unit price")
	ErrUnit = errs.Define(ID, "NO_CONVERSION", errs.FailedPrecondition, 409, errs.RetryNo,
		"The unit does not convert to the product's base unit")
	ErrCode = errs.Define(ID, "CODE_TAKEN", errs.AlreadyExists, 409, errs.RetryNo, "The code is taken")
	// the billing source (protocol 05)
	ErrContent = errs.Define(ID, "CONTENT_CONFLICT", errs.AlreadyExists, 409, errs.RetryNo,
		"The ID was used before with other content")
	ErrClaim = errs.Define(ID, "CLAIM_STATE", errs.FailedPrecondition, 409, errs.RetryNo,
		"The claim's state does not allow this")
)

var (
	UUIDForm     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	CurrencyForm = regexp.MustCompile(`^[A-Z]{3}$`)
	DateForm     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	CodeForm     = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]{0,31}$`)
)

// DocFacts are the facts of one document's three dimensions; a document without a department has none (security
// 07: no department never matches a tree, it never widens to the whole organization).
func DocFacts(legalEntity string, departmentPath *string, owner string) sdkauthz.Facts {
	dept := sdkauthz.Null
	if departmentPath != nil {
		dept = sdkauthz.Value(*departmentPath)
	}
	return sdkauthz.Facts{DimLegalEntity: sdkauthz.Value(legalEntity), DimDepartment: dept, DimOwner: sdkauthz.Value(owner)}
}

// Check decides op on a record with the given facts and returns the context to work in and the acting principal.
func Check(c *httpx.Context, op string, facts sdkauthz.Facts) (context.Context, string, error) {
	s, _ := sdkauthz.From(c.Ctx())
	ctx, _, err := s.Check(c.Ctx(), op, facts)
	if err != nil {
		return ctx, "", err
	}
	u, _ := identity.UserFrom(ctx)
	return ctx, u.Subject, nil
}

func Invalid(field, why string) error {
	return errs.New(errs.ReasonRequestInvalid, why).WithViolations(errs.Violation{Field: field, Reason: "INVALID", Description: why})
}

func NotFound(what string) error { return errs.New(errs.ReasonNotFound, what+" not found") }

// Decimal checks a non-negative decimal string of at most scale places and returns its canonical form.
func Decimal(field, v string, scale int32, positive bool) (string, error) {
	d, err := money.Parse(v)
	if err != nil || d.Sign() < 0 || positive && d.Sign() == 0 || -d.Exponent > scale {
		return "", Invalid(field, field+" is a decimal string with at most "+strconv.Itoa(int(scale))+" places")
	}
	d.Reduce(d)
	return d.Text('f'), nil
}

// Change records the business change with the record's new state (protocol 03), in the transaction that made it.
func Change(m *Base, ctx context.Context, tx db.Tx, actor, op, typ, id string, after any) error {
	_, err := m.RT.Audit.Write(ctx, tx, audit.Record{Category: audit.BusinessChange, Operation: op, Outcome: audit.Succeeded,
		Actor: actor, Target: audit.TargetOf(typ, id, after), Changes: map[string]any{"after": after}})
	return err
}

// Emit writes an event into the outbox, in the transaction that changed the record (protocol 02, S2).
func Emit(m *Base, ctx context.Context, tx db.Tx, typ, id string, version int64, legalEntity string, data any) error {
	_, err := m.RT.Events.Emit(ctx, tx, Stream, events.Event{Type: typ, RecordID: id, Version: version,
		Projection: events.Full, LegalEntity: legalEntity, Data: data})
	return err
}

// Times is a × b rounded half away from zero to scale places.
func Times(a, b string, scale int32) string {
	x, _, _ := apd.NewFromString(a)
	y, _, _ := apd.NewFromString(b)
	var r apd.Decimal
	_, _ = apd.BaseContext.WithPrecision(80).Mul(&r, x, y)
	return money.Format(&r, scale)
}
