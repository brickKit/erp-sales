package documents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	masterdata "github.com/brickKit/erp-sales/v3/internal/masterdata"
	orgunits "github.com/brickKit/erp-sales/v3/internal/orgunits"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/list"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/identity"
)

// What quotations and orders share: the header's evidence and checks, numbers, readers, and a draft's change.

// departmentOf checks a document's department: in the projection (or mdm/org), active, of the document's legal
// entity. It answers the department's ID and path; no department answers nils.
func departmentOf(m *core.Base, ctx context.Context, id *string, legalEntity string) (*string, *string, error) {
	if id == nil {
		return nil, nil, nil
	}
	if !core.UUIDForm.MatchString(*id) {
		return nil, nil, core.Invalid("department_id", "department_id is a department's ID")
	}
	u, ok, err := orgunits.Department(m, ctx, *id)
	if err != nil {
		return nil, nil, err
	}
	if !ok || u.State != "active" || u.LegalEntityID != legalEntity {
		return nil, nil, core.ErrDepartment.New("department " + *id + " is not an active department of the legal entity")
	}
	return id, &u.Path, nil
}

// Prepare checks the header of a new document, decides op on its facts and gathers its evidence; it answers the
// context to work in and the acting principal.
func Prepare(m *core.Base, c *httpx.Context, op, leID, customerID, currency string, owner, dept *string,
	lines []LineIn) (context.Context, string, head, lineEvidence, error) {
	var h head
	var ev lineEvidence
	switch {
	case !core.UUIDForm.MatchString(leID):
		return nil, "", h, ev, core.Invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !core.UUIDForm.MatchString(customerID):
		return nil, "", h, ev, core.Invalid("customer_id", "customer_id is a partner's ID")
	case !core.CurrencyForm.MatchString(currency):
		return nil, "", h, ev, core.Invalid("currency", "currency is an ISO 4217 code")
	case owner != nil && !core.UUIDForm.MatchString(*owner):
		return nil, "", h, ev, core.Invalid("owner_id", "owner_id is a principal's ID")
	}
	if err := checkLines(lines); err != nil {
		return nil, "", h, ev, err
	}
	h.Currency = currency
	if owner != nil {
		h.Owner = *owner
	} else {
		u, _ := identity.UserFrom(c.Ctx())
		h.Owner = u.Subject
	}
	var err error
	if h.DeptID, h.DeptPath, err = departmentOf(m, c.Ctx(), dept, leID); err != nil {
		return nil, "", h, ev, err
	}
	ctx, actor, err := core.Check(c, op, core.DocFacts(leID, h.DeptPath, h.Owner))
	if err != nil {
		return nil, "", h, ev, err
	}
	if h.Le, err = masterdata.LegalEntityOf(m, ctx, leID); err != nil {
		return nil, "", h, ev, err
	}
	if h.scale, err = masterdata.CurrencyScale(m, ctx, currency); err != nil {
		return nil, "", h, ev, err
	}
	if h.Cust, err = masterdata.CustomerOf(m, ctx, customerID, leID); err != nil {
		return nil, "", h, ev, err
	}
	ev, err = evidenceOf(m, ctx, leID, lines)
	return ctx, actor, h, ev, err
}

// Number takes a document number of a series that allows gaps: <prefix>-<legal entity code>-<year>-<six digits>.
func Number(m *core.Base, ctx context.Context, prefix string, le masterdata.LegalEntity, date string) (string, error) {
	year := date[:4]
	n, err := m.RT.DB.NextGappedNumber(ctx, db.Series{LegalEntity: le.ID, Code: prefix, Period: year})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s-%06d", prefix, le.Code, year, n), nil
}

func IDResult(id string) idem.Outcome {
	b, _ := json.Marshal(map[string]string{"id": id})
	return idem.Outcome{Status: 200, Result: b}
}

func ResultID(out idem.Outcome) string {
	var r struct{ ID string }
	_ = json.Unmarshal(out.Result, &r)
	return r.ID
}

// the readers answer NOT_FOUND for an ID of the wrong form or no row; the caller then checks visibility.

func Quotation(m *core.Base, ctx context.Context, id string) (core.Quotation, error) {
	var q core.Quotation
	if !core.UUIDForm.MatchString(id) {
		return q, core.NotFound("quotation")
	}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		q, err = core.GetQuotation(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, core.ErrNone) {
		return q, core.NotFound("quotation")
	}
	return q, err
}

func Order(m *core.Base, ctx context.Context, id string) (core.Order, error) {
	var o core.Order
	if !core.UUIDForm.MatchString(id) {
		return o, core.NotFound("order")
	}
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		o, err = core.GetOrder(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, core.ErrNone) {
		return o, core.NotFound("order")
	}
	return o, err
}

func EmitQuotation(m *core.Base, ctx context.Context, tx db.Tx, id string) (core.Quotation, error) {
	q, err := core.GetQuotation(ctx, tx, id, false)
	if err != nil {
		return q, err
	}
	return q, core.Emit(m, ctx, tx, core.QuotationChanged, q.ID, q.Version, q.LegalEntityID, q)
}

// PatchIn is a draft's change (QuotationPatch and OrderPatch); lines, when given, replace the lines whole.
type PatchIn struct {
	OwnerID      *string  `json:"owner_id"`
	DepartmentID *string  `json:"department_id"`
	ValidUntil   *string  `json:"valid_until"`
	BusinessDate *string  `json:"business_date"`
	Lines        []LineIn `json:"lines"`
	Version      *int64   `json:"version"`
}

// PatchHead decides a draft's change: op on the facts it has, and on the facts it will have when the owner or the
// department changes; then the evidence of new lines.
func PatchHead(m *core.Base, c *httpx.Context, op string, in *PatchIn, le, currency, customerID, owner string,
	deptID, deptPath *string) (context.Context, string, head, lineEvidence, error) {
	h := head{Currency: currency, Owner: owner, DeptID: deptID, DeptPath: deptPath}
	var ev lineEvidence
	ctx, actor, err := core.Check(c, op, core.DocFacts(le, deptPath, owner))
	if err != nil {
		return nil, "", h, ev, err
	}
	if in.OwnerID != nil {
		if !core.UUIDForm.MatchString(*in.OwnerID) {
			return nil, "", h, ev, core.Invalid("owner_id", "owner_id is a principal's ID")
		}
		h.Owner = *in.OwnerID
	}
	if in.DepartmentID != nil {
		if h.DeptID, h.DeptPath, err = departmentOf(m, ctx, in.DepartmentID, le); err != nil {
			return nil, "", h, ev, err
		}
	}
	if h.Owner != owner || in.DepartmentID != nil {
		if _, _, err := core.Check(c, op, core.DocFacts(le, h.DeptPath, h.Owner)); err != nil {
			return nil, "", h, ev, err
		}
	}
	if in.Lines == nil {
		return ctx, actor, h, ev, nil
	}
	if err := checkLines(in.Lines); err != nil {
		return nil, "", h, ev, err
	}
	if h.Le, err = masterdata.LegalEntityOf(m, ctx, le); err != nil {
		return nil, "", h, ev, err
	}
	if h.scale, err = masterdata.CurrencyScale(m, ctx, currency); err != nil {
		return nil, "", h, ev, err
	}
	if _, err := masterdata.CustomerOf(m, ctx, customerID, le); err != nil {
		return nil, "", h, ev, err
	}
	ev, err = evidenceOf(m, ctx, le, in.Lines)
	return ctx, actor, h, ev, err
}

func DocColumns(extra ...list.Column) []list.Column {
	return append([]list.Column{
		{Field: "created_at", Column: "created_at", Kind: list.Time, Sortable: true},
		{Field: "legal_entity_id", Column: "legal_entity_id", Kind: list.UUID, Filterable: true},
		{Field: "customer", Column: "customer_id", Kind: list.UUID, Filterable: true},
		{Field: "owner_id", Column: "owner_id", Kind: list.Text, Filterable: true},
		{Field: "department_id", Column: "department_id", Kind: list.UUID, Filterable: true},
		{Field: "state", Column: "state", Kind: list.Text, Filterable: true}}, extra...)
}

var NumberCustomer = []list.SearchColumn{{Name: "number_customer", Column: "search", Fields: []string{"number", "customer"}}}

var newestFirst = []list.Order{{Field: "created_at", Desc: true}}

// Filters turns the given query parameters (parameter → field) into equality filters, checking the ID ones.
func Filters(c *httpx.Context, params map[string]string) ([]list.Filter, error) {
	var f []list.Filter
	for p, field := range params {
		v := c.Query(p)
		if v == "" {
			continue
		}
		if p != "state" && p != "to_deliver" && p != "to_bill" && !core.UUIDForm.MatchString(v) {
			return nil, core.Invalid(p, p+" is an ID")
		}
		if (p == "to_deliver" || p == "to_bill") && v != "true" && v != "false" {
			return nil, core.Invalid(p, p+" is true or false")
		}
		f = append(f, list.Filter{Field: field, Op: "eq", Values: []string{v}})
	}
	return f, nil
}

func Build(c *httpx.Context, spec list.Spec, f []list.Filter) (context.Context, *list.Query, error) {
	limit, err := list.ParseLimit(c.Query("limit"))
	if err != nil {
		return nil, nil, err
	}
	s, _ := sdkauthz.From(c.Ctx())
	return list.Build(c.Ctx(), s, spec, list.Request{Filters: f, Q: c.Query("q"), Limit: limit, Cursor: c.Query("cursor"),
		Sort: newestFirst})
}

var DocParams = map[string]string{"legal_entity_id": "legal_entity_id", "customer_id": "customer", "owner_id": "owner_id",
	"department_id": "department_id", "state": "state"}
