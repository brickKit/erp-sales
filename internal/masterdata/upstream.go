package masterdata

import (
	"context"
	"encoding/json"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"

	"github.com/brickKit/be-sdk-go/v3/calls"
	"github.com/brickKit/be-sdk-go/v3/errs"
	currencyv3 "github.com/brickKit/mdm-currency/contract/v3/gen/go/be/mdm/currency/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	partnerv3 "github.com/brickKit/mdm-partner/contract/v3/gen/go/be/mdm/partner/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
	"google.golang.org/grpc"
)

// Every call here runs outside a transaction (protocol P4.20): evidence first, then the transaction. Unreachable is
// never "does not exist" (protocol 07): it answers DEPENDENCY_UNAVAILABLE.

func Conn(m *core.Base, id string) (*grpc.ClientConn, error) {
	c, ok := m.RT.GRPC(id)
	if !ok {
		return nil, errs.New(errs.ReasonDependencyUnavailable, id+" is not reachable")
	}
	return c, nil
}

// LegalEntity is what sales needs of a legal entity.
type LegalEntity struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	State        string `json:"state"`
	Timezone     string `json:"timezone"`
	BaseCurrency string `json:"base_currency"`
}

func LegalEntityOf(m *core.Base, ctx context.Context, id string) (LegalEntity, error) {
	c, err := Conn(m, core.Org)
	if err != nil {
		return LegalEntity{}, err
	}
	res, err := orgv3.NewSourceServiceClient(c).Read(ctx, &orgv3.ReadRequest{
		Aggregate: orgv3.Aggregate_AGGREGATE_LEGAL_ENTITY, Ids: []string{id}}, calls.Idempotent())
	if err != nil {
		return LegalEntity{}, err
	}
	var le LegalEntity
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != orgv3.ReadState_READ_STATE_EXISTS {
		return le, core.ErrLegalEntity.New("no legal entity " + id)
	}
	if err := json.Unmarshal(res.GetResults()[0].GetData(), &le); err != nil {
		return le, err
	}
	if le.State != "active" {
		return le, core.ErrLegalEntity.New("the legal entity is " + le.State)
	}
	return le, nil
}

// OrgUnit is a department as the projection keeps it (mdm.org_unit.changed.v1).
type OrgUnit struct {
	ID            string `json:"id"`
	LegalEntityID string `json:"legal_entity_id"`
	Path          string `json:"path"`
	State         string `json:"state"`
}

// OrgUnitRead reads one department from mdm/org's source method, with its version and the source's identity.
func OrgUnitRead(m *core.Base, ctx context.Context, id string) (OrgUnit, int64, string, string, bool, error) {
	c, err := Conn(m, core.Org)
	if err != nil {
		return OrgUnit{}, 0, "", "", false, err
	}
	res, err := orgv3.NewSourceServiceClient(c).Read(ctx, &orgv3.ReadRequest{
		Aggregate: orgv3.Aggregate_AGGREGATE_ORG_UNIT, Ids: []string{id}}, calls.Idempotent())
	if err != nil {
		return OrgUnit{}, 0, "", "", false, err
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != orgv3.ReadState_READ_STATE_EXISTS {
		return OrgUnit{}, 0, res.GetStamp().GetInstance(), res.GetStamp().GetEpoch(), false, nil
	}
	var u OrgUnit
	r := res.GetResults()[0]
	return u, r.GetVersion(), res.GetStamp().GetInstance(), res.GetStamp().GetEpoch(), true, json.Unmarshal(r.GetData(), &u)
}

// Customer is the evidence and the snapshot of a customer for a document.
type Customer struct {
	ID          string
	Name        string
	TaxID       *string
	EvaluatedAt time.Time
}

// CustomerOf asks mdm/partner whether the partner may be sold to in the legal entity now, and reads its name and
// tax ID for the document's snapshot (protocol 07).
func CustomerOf(m *core.Base, ctx context.Context, partner, legalEntity string) (Customer, error) {
	c, err := Conn(m, core.Partner)
	if err != nil {
		return Customer{}, err
	}
	asked := time.Now()
	e, err := partnerv3.NewPartnerServiceClient(c).CheckEligibility(ctx, &partnerv3.CheckEligibilityRequest{
		PartnerId: partner, LegalEntityId: legalEntity, Purpose: partnerv3.Purpose_PURPOSE_SELL}, calls.Idempotent())
	if err != nil {
		return Customer{}, err
	}
	if !e.GetEligible() {
		return Customer{}, core.ErrIneligible.New("the customer is not eligible: " + e.GetReason())
	}
	res, err := partnerv3.NewSourceServiceClient(c).Read(ctx, &partnerv3.ReadRequest{
		Aggregate: partnerv3.Aggregate_AGGREGATE_PARTNER, Ids: []string{partner}}, calls.Idempotent())
	if err != nil {
		return Customer{}, err
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != partnerv3.ReadState_READ_STATE_EXISTS {
		return Customer{}, core.ErrIneligible.New("no partner " + partner)
	}
	var p struct {
		Name  string  `json:"name"`
		TaxID *string `json:"tax_id"`
	}
	if err := json.Unmarshal(res.GetResults()[0].GetData(), &p); err != nil {
		return Customer{}, err
	}
	return Customer{ID: partner, Name: p.Name, TaxID: p.TaxID, EvaluatedAt: asked}, nil
}

// ProductInfo is the evidence and the snapshot of a product for a document line.
type ProductInfo struct {
	ID          string
	Code        string
	Name        string
	BaseUnit    string
	Stocked     bool
	EvaluatedAt time.Time
}

// ProductsOf asks mdm/product for each product's eligibility to sell in the legal entity and reads its code, name
// and base unit.
func ProductsOf(m *core.Base, ctx context.Context, ids []string, legalEntity string) (map[string]ProductInfo, error) {
	c, err := Conn(m, core.Product)
	if err != nil {
		return nil, err
	}
	uniq := map[string]bool{}
	var ask []string
	for _, id := range ids {
		if !uniq[id] {
			uniq[id] = true
			ask = append(ask, id)
		}
	}
	out := map[string]ProductInfo{}
	ps := productv3.NewProductServiceClient(c)
	for _, id := range ask {
		asked := time.Now()
		e, err := ps.CheckEligibility(ctx, &productv3.CheckEligibilityRequest{ProductId: id, LegalEntityId: legalEntity,
			Purpose: productv3.Purpose_PURPOSE_SELL}, calls.Idempotent())
		if err != nil {
			return nil, err
		}
		if !e.GetEligible() {
			return nil, core.ErrIneligible.New("product " + id + " is not eligible to sell: " + e.GetReason())
		}
		out[id] = ProductInfo{ID: id, Stocked: e.GetStocked(), EvaluatedAt: asked}
	}
	res, err := productv3.NewSourceServiceClient(c).Read(ctx, &productv3.ReadRequest{
		Aggregate: productv3.Aggregate_AGGREGATE_PRODUCT, Ids: ask}, calls.Idempotent())
	if err != nil {
		return nil, err
	}
	for _, r := range res.GetResults() {
		var p struct {
			ID       string `json:"id"`
			Code     string `json:"code"`
			Name     string `json:"name"`
			BaseUnit string `json:"base_unit"`
		}
		if r.GetState() != productv3.ReadState_READ_STATE_EXISTS || json.Unmarshal(r.GetData(), &p) != nil {
			continue
		}
		info := out[p.ID]
		info.Code, info.Name, info.BaseUnit = p.Code, p.Name, p.BaseUnit
		out[p.ID] = info
	}
	return out, nil
}

// Convert answers a quantity of the product in its base unit (rounded to the base unit's decimals by mdm/product) and
// the factor: how many base units one unit is.
func Convert(m *core.Base, ctx context.Context, product, quantity, unit, baseUnit string) (string, string, error) {
	if unit == baseUnit {
		return quantity, "1", nil
	}
	c, err := Conn(m, core.Product)
	if err != nil {
		return "", "", err
	}
	res, err := productv3.NewProductServiceClient(c).Convert(ctx, &productv3.ConvertRequest{ProductId: product,
		Quantity: quantity, FromUnit: unit, ToUnit: baseUnit}, calls.Idempotent())
	if err != nil {
		if errs.Is(err, core.Product, "NO_CONVERSION") {
			return "", "", core.ErrUnit.New("unit " + unit + " does not convert to " + baseUnit + " for product " + product)
		}
		return "", "", err
	}
	return res.GetQuantity(), res.GetFactor(), nil
}

// Rate is an exchange rate's snapshot (protocol 14).
type Rate struct {
	Rate, Date, Type, RecordID string
	RecordVersion              int64
}

// SpotRate is the spot rate from the order's currency to the legal entity's base currency on the business date.
func SpotRate(m *core.Base, ctx context.Context, from, to, date string) (Rate, error) {
	c, err := Conn(m, core.Currency)
	if err != nil {
		return Rate{}, err
	}
	res, err := currencyv3.NewRateServiceClient(c).EffectiveRate(ctx, &currencyv3.EffectiveRateRequest{RateType: "spot",
		Date: date, FromCurrency: from, ToCurrency: to}, calls.Idempotent())
	if err != nil {
		if errs.Is(err, errs.PlatformDomain, errs.ReasonNotFound) {
			return Rate{}, core.ErrNoRate.New("no spot rate from " + from + " to " + to + " on " + date)
		}
		return Rate{}, err
	}
	return Rate{Rate: res.GetRate(), Date: res.GetRateDate(), Type: res.GetRateType(), RecordID: res.GetId(),
		RecordVersion: res.GetVersion()}, nil
}

// CurrencyScale is an active currency's minor unit.
func CurrencyScale(m *core.Base, ctx context.Context, code string) (int32, error) {
	c, err := Conn(m, core.Currency)
	if err != nil {
		return 0, err
	}
	res, err := currencyv3.NewSourceServiceClient(c).Read(ctx, &currencyv3.ReadRequest{
		Aggregate: currencyv3.Aggregate_AGGREGATE_CURRENCY, Ids: []string{code}}, calls.Idempotent())
	if err != nil {
		return 0, err
	}
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetState() != currencyv3.ReadState_READ_STATE_EXISTS {
		return 0, core.ErrCurrency.New("no currency " + code)
	}
	var cur struct {
		MinorUnit int32 `json:"minor_unit"`
		Active    bool  `json:"active"`
	}
	if err := json.Unmarshal(res.GetResults()[0].GetData(), &cur); err != nil {
		return 0, err
	}
	if !cur.Active {
		return 0, core.ErrCurrency.New("currency " + code + " is not active")
	}
	return cur.MinorUnit, nil
}
