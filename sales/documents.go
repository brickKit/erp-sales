package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/identity"
)

// What quotations and orders share: the header's evidence and checks, numbers, readers, and a draft's change.

// departmentOf checks a document's department: in the projection (or mdm/org), active, of the document's legal
// entity. It answers the department's ID and path; no department answers nils.
func (m *Module) departmentOf(ctx context.Context, id *string, legalEntity string) (*string, *string, error) {
	if id == nil {
		return nil, nil, nil
	}
	if !uuidForm.MatchString(*id) {
		return nil, nil, invalid("department_id", "department_id is a department's ID")
	}
	u, ok, err := m.department(ctx, *id)
	if err != nil {
		return nil, nil, err
	}
	if !ok || u.State != "active" || u.LegalEntityID != legalEntity {
		return nil, nil, errDepartment.New("department " + *id + " is not an active department of the legal entity")
	}
	return id, &u.Path, nil
}

// prepare checks the header of a new document, decides op on its facts and gathers its evidence; it answers the
// context to work in and the acting principal.
func (m *Module) prepare(c *httpx.Context, op, leID, customerID, currency string, owner, dept *string,
	lines []lineIn) (context.Context, string, head, lineEvidence, error) {
	var h head
	var ev lineEvidence
	switch {
	case !uuidForm.MatchString(leID):
		return nil, "", h, ev, invalid("legal_entity_id", "legal_entity_id is a legal entity's ID")
	case !uuidForm.MatchString(customerID):
		return nil, "", h, ev, invalid("customer_id", "customer_id is a partner's ID")
	case !currencyForm.MatchString(currency):
		return nil, "", h, ev, invalid("currency", "currency is an ISO 4217 code")
	case owner != nil && !uuidForm.MatchString(*owner):
		return nil, "", h, ev, invalid("owner_id", "owner_id is a principal's ID")
	}
	if err := checkLines(lines); err != nil {
		return nil, "", h, ev, err
	}
	h.currency = currency
	if owner != nil {
		h.owner = *owner
	} else {
		u, _ := identity.UserFrom(c.Ctx())
		h.owner = u.Subject
	}
	var err error
	if h.deptID, h.deptPath, err = m.departmentOf(c.Ctx(), dept, leID); err != nil {
		return nil, "", h, ev, err
	}
	ctx, actor, err := check(c, op, docFacts(leID, h.deptPath, h.owner))
	if err != nil {
		return nil, "", h, ev, err
	}
	if h.le, err = m.legalEntityOf(ctx, leID); err != nil {
		return nil, "", h, ev, err
	}
	if h.scale, err = m.currencyScale(ctx, currency); err != nil {
		return nil, "", h, ev, err
	}
	if h.cust, err = m.customerOf(ctx, customerID, leID); err != nil {
		return nil, "", h, ev, err
	}
	ev, err = m.evidenceOf(ctx, leID, lines)
	return ctx, actor, h, ev, err
}

// number takes a document number of a series that allows gaps: <prefix>-<legal entity code>-<year>-<six digits>.
func (m *Module) number(ctx context.Context, prefix string, le legalEntity, date string) (string, error) {
	year := date[:4]
	n, err := m.rt.DB.NextGappedNumber(ctx, db.Series{LegalEntity: le.ID, Code: prefix, Period: year})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s-%06d", prefix, le.Code, year, n), nil
}

func idResult(id string) idem.Outcome {
	b, _ := json.Marshal(map[string]string{"id": id})
	return idem.Outcome{Status: 200, Result: b}
}

func resultID(out idem.Outcome) string {
	var r struct{ ID string }
	_ = json.Unmarshal(out.Result, &r)
	return r.ID
}

// the readers answer NOT_FOUND for an ID of the wrong form or no row; the caller then checks visibility.

func (m *Module) quotation(ctx context.Context, id string) (Quotation, error) {
	var q Quotation
	if !uuidForm.MatchString(id) {
		return q, notFound("quotation")
	}
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		q, err = getQuotation(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) {
		return q, notFound("quotation")
	}
	return q, err
}

func (m *Module) order(ctx context.Context, id string) (Order, error) {
	var o Order
	if !uuidForm.MatchString(id) {
		return o, notFound("order")
	}
	err := m.rt.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		o, err = getOrder(ctx, tx, id, false)
		return err
	})
	if errors.Is(err, errNone) {
		return o, notFound("order")
	}
	return o, err
}

func (m *Module) emitQuotation(ctx context.Context, tx db.Tx, id string) (Quotation, error) {
	q, err := getQuotation(ctx, tx, id, false)
	if err != nil {
		return q, err
	}
	return q, m.emit(ctx, tx, QuotationChanged, q.ID, q.Version, q.LegalEntityID, q)
}

// patchIn is a draft's change (QuotationPatch and OrderPatch); lines, when given, replace the lines whole.
type patchIn struct {
	OwnerID      *string  `json:"owner_id"`
	DepartmentID *string  `json:"department_id"`
	ValidUntil   *string  `json:"valid_until"`
	BusinessDate *string  `json:"business_date"`
	Lines        []lineIn `json:"lines"`
	Version      *int64   `json:"version"`
}

// patchHead decides a draft's change: op on the facts it has, and on the facts it will have when the owner or the
// department changes; then the evidence of new lines.
func (m *Module) patchHead(c *httpx.Context, op string, in *patchIn, le, currency, customerID, owner string,
	deptID, deptPath *string) (context.Context, string, head, lineEvidence, error) {
	h := head{currency: currency, owner: owner, deptID: deptID, deptPath: deptPath}
	var ev lineEvidence
	ctx, actor, err := check(c, op, docFacts(le, deptPath, owner))
	if err != nil {
		return nil, "", h, ev, err
	}
	if in.OwnerID != nil {
		if !uuidForm.MatchString(*in.OwnerID) {
			return nil, "", h, ev, invalid("owner_id", "owner_id is a principal's ID")
		}
		h.owner = *in.OwnerID
	}
	if in.DepartmentID != nil {
		if h.deptID, h.deptPath, err = m.departmentOf(ctx, in.DepartmentID, le); err != nil {
			return nil, "", h, ev, err
		}
	}
	if h.owner != owner || in.DepartmentID != nil {
		if _, _, err := check(c, op, docFacts(le, h.deptPath, h.owner)); err != nil {
			return nil, "", h, ev, err
		}
	}
	if in.Lines == nil {
		return ctx, actor, h, ev, nil
	}
	if err := checkLines(in.Lines); err != nil {
		return nil, "", h, ev, err
	}
	if h.le, err = m.legalEntityOf(ctx, le); err != nil {
		return nil, "", h, ev, err
	}
	if h.scale, err = m.currencyScale(ctx, currency); err != nil {
		return nil, "", h, ev, err
	}
	if _, err := m.customerOf(ctx, customerID, le); err != nil {
		return nil, "", h, ev, err
	}
	ev, err = m.evidenceOf(ctx, le, in.Lines)
	return ctx, actor, h, ev, err
}
