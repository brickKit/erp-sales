//go:build integration

package sales

// The stand-ins of the integration test: the master data components and the two participants of protocol 04, each
// speaking its published contract. The participants can be told to refuse, to lose an answer once, to be down or to
// let a hold expire, so the test can drive every row of the confirmation's tables.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	creditv3 "github.com/brickKit/erp-credit/contract/v3/gen/go/be/erp/credit/v3"
	inventoryv3 "github.com/brickKit/erp-inventory/contract/v3/gen/go/be/erp/inventory/v3"
	currencyv3 "github.com/brickKit/mdm-currency/contract/v3/gen/go/be/mdm/currency/v3"
	orgv3 "github.com/brickKit/mdm-org/contract/v3/gen/go/be/mdm/org/v3"
	partnerv3 "github.com/brickKit/mdm-partner/contract/v3/gen/go/be/mdm/partner/v3"
	productv3 "github.com/brickKit/mdm-product/contract/v3/gen/go/be/mdm/product/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var down = status.Error(codes.Unavailable, "the stand-in is down")

// --- mdm/org: legal entities and departments ---

type orgStandIn struct {
	orgv3.UnimplementedSourceServiceServer
	mu    sync.Mutex
	les   map[string]map[string]any
	units map[string]map[string]any
}

var orgStamp = &orgv3.Stamp{Instance: "org-it", Epoch: "1.1"}

func (o *orgStandIn) Read(_ context.Context, in *orgv3.ReadRequest) (*orgv3.ReadResponse, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	src := o.les
	if in.GetAggregate() == orgv3.Aggregate_AGGREGATE_ORG_UNIT {
		src = o.units
	}
	out := &orgv3.ReadResponse{Stamp: orgStamp}
	for _, id := range in.GetIds() {
		r := &orgv3.ReadResult{Id: id, State: orgv3.ReadState_READ_STATE_PURGED}
		if v, ok := src[id]; ok {
			data, _ := json.Marshal(v)
			r = &orgv3.ReadResult{Id: id, State: orgv3.ReadState_READ_STATE_EXISTS, Version: v["version"].(int64), Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

func (o *orgStandIn) Snapshot(_ context.Context, in *orgv3.SnapshotRequest) (*orgv3.SnapshotResponse, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := &orgv3.SnapshotResponse{Stamp: orgStamp}
	if in.GetAggregate() != orgv3.Aggregate_AGGREGATE_ORG_UNIT || in.GetCursor() != "" {
		return out, nil
	}
	for id, v := range o.units {
		data, _ := json.Marshal(v)
		out.Records = append(out.Records, &orgv3.Record{Id: id, Version: v["version"].(int64), Data: data})
	}
	return out, nil
}

// --- mdm/partner ---

type partnerStandIn struct {
	partnerv3.UnimplementedSourceServiceServer
	partnerv3.UnimplementedPartnerServiceServer
	names   map[string]string
	blocked map[string]bool
}

func (p *partnerStandIn) CheckEligibility(_ context.Context, in *partnerv3.CheckEligibilityRequest) (*partnerv3.CheckEligibilityResponse, error) {
	if _, ok := p.names[in.GetPartnerId()]; !ok {
		return &partnerv3.CheckEligibilityResponse{Reason: "not_found"}, nil
	}
	if p.blocked[in.GetPartnerId()] {
		return &partnerv3.CheckEligibilityResponse{Reason: "partner_suspended"}, nil
	}
	return &partnerv3.CheckEligibilityResponse{Eligible: true, Reason: "eligible"}, nil
}

func (p *partnerStandIn) Read(_ context.Context, in *partnerv3.ReadRequest) (*partnerv3.ReadResponse, error) {
	out := &partnerv3.ReadResponse{Stamp: &partnerv3.Stamp{}}
	for _, id := range in.GetIds() {
		r := &partnerv3.ReadResult{Id: id, State: partnerv3.ReadState_READ_STATE_PURGED}
		if n, ok := p.names[id]; ok {
			data, _ := json.Marshal(map[string]any{"id": id, "name": n, "tax_id": "91310000-" + id[:4]})
			r = &partnerv3.ReadResult{Id: id, State: partnerv3.ReadState_READ_STATE_EXISTS, Version: 1, Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// --- mdm/product ---

type productInfoIT struct {
	Code, Name, BaseUnit string
	Stocked              bool
	Box                  string // the factor of BX to the base unit, when it has one
}

type productStandIn struct {
	productv3.UnimplementedSourceServiceServer
	productv3.UnimplementedProductServiceServer
	known map[string]productInfoIT
}

func (p *productStandIn) CheckEligibility(_ context.Context, in *productv3.CheckEligibilityRequest) (*productv3.CheckEligibilityResponse, error) {
	v, ok := p.known[in.GetProductId()]
	if !ok {
		return &productv3.CheckEligibilityResponse{Reason: "not_found"}, nil
	}
	return &productv3.CheckEligibilityResponse{Eligible: true, Reason: "eligible", Stocked: v.Stocked}, nil
}

func (p *productStandIn) Convert(_ context.Context, in *productv3.ConvertRequest) (*productv3.ConvertResponse, error) {
	v := p.known[in.GetProductId()]
	if in.GetFromUnit() != "BX" || v.Box == "" || in.GetToUnit() != v.BaseUnit {
		return nil, status.Error(codes.FailedPrecondition, "NO_CONVERSION")
	}
	return &productv3.ConvertResponse{Quantity: times(in.GetQuantity(), v.Box, 0), Factor: v.Box}, nil
}

func (p *productStandIn) Read(_ context.Context, in *productv3.ReadRequest) (*productv3.ReadResponse, error) {
	out := &productv3.ReadResponse{Stamp: &productv3.Stamp{}}
	for _, id := range in.GetIds() {
		r := &productv3.ReadResult{Id: id, State: productv3.ReadState_READ_STATE_PURGED}
		if v, ok := p.known[id]; ok {
			data, _ := json.Marshal(map[string]any{"id": id, "code": v.Code, "name": v.Name, "base_unit": v.BaseUnit})
			r = &productv3.ReadResult{Id: id, State: productv3.ReadState_READ_STATE_EXISTS, Version: 1, Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// --- mdm/currency: CNY and USD, one USD is 7.1 CNY ---

type currencyStandIn struct {
	currencyv3.UnimplementedSourceServiceServer
	currencyv3.UnimplementedRateServiceServer
}

func (currencyStandIn) Read(_ context.Context, in *currencyv3.ReadRequest) (*currencyv3.ReadResponse, error) {
	out := &currencyv3.ReadResponse{Stamp: &currencyv3.Stamp{}}
	for _, code := range in.GetIds() {
		r := &currencyv3.ReadResult{Id: code, State: currencyv3.ReadState_READ_STATE_PURGED}
		if code == "CNY" || code == "USD" {
			data, _ := json.Marshal(map[string]any{"code": code, "minor_unit": 2, "active": true})
			r = &currencyv3.ReadResult{Id: code, State: currencyv3.ReadState_READ_STATE_EXISTS, Version: 1, Data: data}
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

const rateID = "01a15ab0-0000-7000-8000-00000000c0de"

func (currencyStandIn) EffectiveRate(_ context.Context, in *currencyv3.EffectiveRateRequest) (*currencyv3.EffectiveRateResponse, error) {
	if in.GetFromCurrency() != "USD" || in.GetToCurrency() != "CNY" {
		return nil, status.Error(codes.NotFound, "no rate")
	}
	return &currencyv3.EffectiveRateResponse{Id: rateID, Version: 3, Rate: "7.1", RateDate: in.GetDate(), RateType: "spot",
		FromCurrency: "USD", ToCurrency: "CNY"}, nil
}

// --- the participants (protocol 04) ---

// participant keeps branches as protocol 04 says. Modes per command: "" answers, "reject" refuses a hold, "lost"
// applies the command and loses the answer once, "down" answers nothing, "expire" lets a held branch expire on
// confirm.
type participant struct {
	mu       sync.Mutex
	branches map[string]string // branch ID → state
	holds    map[string]int    // how many holds took effect per branch
	content  map[string]any    // what each branch holds
	mode     map[string]string // command → mode
	reason   string            // the reason of a refused hold
}

func newParticipant(reason string) *participant {
	return &participant{branches: map[string]string{}, holds: map[string]int{}, content: map[string]any{},
		mode: map[string]string{}, reason: reason}
}

func (p *participant) set(cmd, mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode[cmd] = mode
}

func (p *participant) state(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.branches[id]
}

// do runs one command; it answers the branch's state (and the reason of a refusal).
func (p *participant) do(cmd, id string, content any) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mode := p.mode[cmd]
	if mode == "down" {
		return "", "", down
	}
	st, ok := p.branches[id]
	reason := ""
	switch cmd {
	case "hold":
		switch {
		case ok:
		case mode == "reject":
			st, reason = "rejected", p.reason
		default:
			st = "held"
			p.holds[id]++
			p.content[id] = content
		}
	case "confirm":
		switch {
		case !ok:
			st = "absent"
		case st == "held" && mode == "expire":
			st = "expired"
		case st == "held":
			st = "committed"
		}
	case "cancel":
		switch {
		case !ok:
			st = "cancelled"
		case st == "held" || st == "committed":
			st = "released"
		}
	case "query":
		if !ok {
			return "absent", "", nil
		}
	}
	if st == "rejected" && reason == "" {
		reason = p.reason
	}
	p.branches[id] = st
	if mode == "lost" {
		delete(p.mode, cmd) // once
		return "", "", down
	}
	return st, reason, nil
}

type inventoryStandIn struct {
	inventoryv3.UnimplementedReservationServiceServer
	inventoryv3.UnimplementedStockServiceServer
	*participant
	issued    map[string]*inventoryv3.IssueRequest
	issueMode string
}

func invBranch(id, st, reason string) *inventoryv3.Branch {
	return &inventoryv3.Branch{BranchId: id, Reason: reason,
		State: inventoryv3.BranchState(inventoryv3.BranchState_value["BRANCH_STATE_"+strings.ToUpper(st)])}
}

func (s *inventoryStandIn) Hold(_ context.Context, in *inventoryv3.HoldRequest) (*inventoryv3.HoldResponse, error) {
	st, why, err := s.do("hold", in.GetBranchId(), in.GetLines())
	return &inventoryv3.HoldResponse{Branch: invBranch(in.GetBranchId(), st, why)}, err
}

func (s *inventoryStandIn) Confirm(_ context.Context, in *inventoryv3.ConfirmRequest) (*inventoryv3.ConfirmResponse, error) {
	st, why, err := s.do("confirm", in.GetBranchId(), nil)
	return &inventoryv3.ConfirmResponse{Branch: invBranch(in.GetBranchId(), st, why)}, err
}

func (s *inventoryStandIn) Cancel(_ context.Context, in *inventoryv3.CancelRequest) (*inventoryv3.CancelResponse, error) {
	st, why, err := s.do("cancel", in.GetBranchId(), nil)
	return &inventoryv3.CancelResponse{Branch: invBranch(in.GetBranchId(), st, why)}, err
}

func (s *inventoryStandIn) Query(_ context.Context, in *inventoryv3.QueryRequest) (*inventoryv3.QueryResponse, error) {
	st, why, err := s.do("query", in.GetBranchId(), nil)
	return &inventoryv3.QueryResponse{Branch: invBranch(in.GetBranchId(), st, why)}, err
}

// Issue is idempotent by the consumption ID; "reject" refuses with a problem, "down" answers nothing.
func (s *inventoryStandIn) Issue(_ context.Context, in *inventoryv3.IssueRequest) (*inventoryv3.IssueResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.issued[in.GetConsumptionId()]; ok {
		_ = prev
		return &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Issued: true}, nil
	}
	switch s.issueMode {
	case "down":
		return nil, down
	case "reject":
		return &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(),
			Problems: []*inventoryv3.IssueProblem{{Line: 0, Reason: "insufficient_batch", Detail: "batch B1"}}}, nil
	}
	s.issued[in.GetConsumptionId()] = in
	return &inventoryv3.IssueResponse{ConsumptionId: in.GetConsumptionId(), Issued: true}, nil
}

func (s *inventoryStandIn) setIssue(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueMode = mode
}

func (s *inventoryStandIn) issue(id string) *inventoryv3.IssueRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued[id]
}

type creditStandIn struct {
	creditv3.UnimplementedReservationServiceServer
	*participant
}

func credBranch(id, st, reason string) *creditv3.Branch {
	return &creditv3.Branch{BranchId: id, Reason: reason,
		State: creditv3.BranchState(creditv3.BranchState_value["BRANCH_STATE_"+strings.ToUpper(st)])}
}

func (s *creditStandIn) Hold(_ context.Context, in *creditv3.HoldRequest) (*creditv3.HoldResponse, error) {
	st, why, err := s.do("hold", in.GetBranchId(), in.GetAmount()+" "+in.GetCurrency())
	return &creditv3.HoldResponse{Branch: credBranch(in.GetBranchId(), st, why)}, err
}

func (s *creditStandIn) Confirm(_ context.Context, in *creditv3.ConfirmRequest) (*creditv3.ConfirmResponse, error) {
	st, why, err := s.do("confirm", in.GetBranchId(), nil)
	return &creditv3.ConfirmResponse{Branch: credBranch(in.GetBranchId(), st, why)}, err
}

func (s *creditStandIn) Cancel(_ context.Context, in *creditv3.CancelRequest) (*creditv3.CancelResponse, error) {
	st, why, err := s.do("cancel", in.GetBranchId(), nil)
	return &creditv3.CancelResponse{Branch: credBranch(in.GetBranchId(), st, why)}, err
}

func (s *creditStandIn) Query(_ context.Context, in *creditv3.QueryRequest) (*creditv3.QueryResponse, error) {
	st, why, err := s.do("query", in.GetBranchId(), nil)
	return &creditv3.QueryResponse{Branch: credBranch(in.GetBranchId(), st, why)}, err
}
