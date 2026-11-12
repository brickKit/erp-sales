package quoterequests

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	core "github.com/brickKit/erp-sales/v3/internal/core"
	documents "github.com/brickKit/erp-sales/v3/internal/documents"
	masterdata "github.com/brickKit/erp-sales/v3/internal/masterdata"

	"github.com/brickKit/be-sdk-go/v3/bizdate"
	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/events"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/brickKit/be-sdk-go/v3/search"
	"github.com/brickKit/be-sdk-go/v3/work"
	"github.com/jackc/pgx/v5"
)

// crm/opportunity's quote requests (design QR01-QR04): received in the subscription's transaction, decided by a job
// that gathers the evidence between transactions.

// JobDraft decides one received request: a quotation draft, or the refusal.
const JobDraft = "draft-quotation"

// request is crm.opportunity.quote_requested.v1 (crm/opportunity's contract), the fields this component uses.
type request struct {
	FactID        string                  `json:"fact_id"`
	OpportunityID string                  `json:"opportunity_id"`
	LegalEntityID string                  `json:"legal_entity_id"`
	CustomerID    string                  `json:"customer_id"`
	Currency      string                  `json:"currency"`
	OwnerID       string                  `json:"owner_id"`
	Lines         []core.RequestedProduct `json:"lines"`
}

type draftArgs struct {
	ID string `json:"id"`
}

// OnRequested keeps a request once, by its fact ID, and queues its decision (QR01). A request this component cannot
// read is logged and dropped: redelivering it would never make it readable.
func OnRequested(m *core.Base, ctx context.Context, tx db.Tx, e events.Received) error {
	var r request
	if err := json.Unmarshal(e.Data, &r); err != nil || !core.UUIDForm.MatchString(r.FactID) ||
		!core.UUIDForm.MatchString(r.OpportunityID) {
		m.RT.Logger.Warn("a quote request that cannot be read is dropped", "event", e.ID, "error", err)
		return nil
	}
	if r.Lines == nil {
		r.Lines = []core.RequestedProduct{}
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO quote_request_in (id, opportunity_id, payload) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`, r.FactID, r.OpportunityID, payload)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = m.RT.Work.Enqueue(ctx, tx, JobDraft, draftArgs{ID: r.FactID})
	return err
}

// DraftJob takes the evidence a quotation needs (QR02) outside any transaction, then makes the draft (QR03) or
// refuses the request (QR04). An unreachable master data component fails the run: it is retried.
func DraftJob(m *core.Base, ctx context.Context, run *work.Run) error {
	var a draftArgs
	if err := json.Unmarshal(run.Args, &a); err != nil {
		return err
	}
	var r request
	var state string
	var payload []byte
	err := m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, payload FROM quote_request_in WHERE id = $1`, a.ID).Scan(&state, &payload)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "received" {
		return nil
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return err
	}
	ev, refusal, err := evidence(m, ctx, r)
	if err != nil {
		return err
	}
	if refusal != nil {
		return refuse(m, ctx, r, refusal)
	}
	return draft(m, ctx, r, ev)
}

type proof struct {
	le   masterdata.LegalEntity
	cust masterdata.Customer
}

// evidence answers the evidence, or the refusal when the legal entity, the currency or the customer is not usable.
func evidence(m *core.Base, ctx context.Context, r request) (proof, *errs.Error, error) {
	var p proof
	var err error
	if !core.UUIDForm.MatchString(r.LegalEntityID) || !core.UUIDForm.MatchString(r.CustomerID) ||
		!core.CurrencyForm.MatchString(r.Currency) || !core.UUIDForm.MatchString(r.OwnerID) {
		return p, core.ErrIneligible.New("the request's legal entity, customer, currency or owner is malformed"), nil
	}
	if p.le, err = masterdata.LegalEntityOf(m, ctx, r.LegalEntityID); err != nil {
		return refusalOf(p, err)
	}
	if _, err = masterdata.CurrencyScale(m, ctx, r.Currency); err != nil {
		return refusalOf(p, err)
	}
	p.cust, err = masterdata.CustomerOf(m, ctx, r.CustomerID, r.LegalEntityID)
	return refusalOf(p, err)
}

func refusalOf(p proof, err error) (proof, *errs.Error, error) {
	if err == nil {
		return p, nil, nil
	}
	for _, d := range []errs.Declared{core.ErrLegalEntity, core.ErrCurrency, core.ErrIneligible} {
		if errs.Is(err, d.Domain, d.Reason) {
			return p, errs.From(err), nil
		}
	}
	return p, nil, err
}

// draft makes the quotation draft: the request's header, no lines, the requested products kept for the user (QR03).
func draft(m *core.Base, ctx context.Context, r request, p proof) error {
	today, err := bizdate.Today(p.le.Timezone)
	if err != nil {
		return err
	}
	number, err := documents.Number(m, ctx, "QT", p.le, today)
	if err != nil {
		return err
	}
	products, err := json.Marshal(r.Lines)
	if err != nil {
		return err
	}
	return m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		if decided, err := lockReceived(ctx, tx, r.FactID); err != nil || decided {
			return err
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO quotation (id, number, legal_entity_id, customer_id, customer_name,
			customer_tax_id, currency, owner_id, opportunity_ref, quote_request_id, requested_products, search, search_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, id, number, p.le.ID, p.cust.ID, p.cust.Name,
			p.cust.TaxID, r.Currency, r.OwnerID, r.OpportunityID, r.FactID, products, search.Text(number, p.cust.Name),
			search.Version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quote_request_in SET state = 'drafted', quotation_id = $2, decided_at = now()
			WHERE id = $1`, r.FactID, id); err != nil {
			return err
		}
		q, err := documents.EmitQuotation(m, ctx, tx, id)
		if err != nil {
			return err
		}
		return core.Change(m, ctx, tx, "", core.OpQuoteEdit, core.Quotations, q.ID, q) // no user: the component acts
	})
}

// refuse records the refusal and publishes it (QR04); the fact ID is the request's.
func refuse(m *core.Base, ctx context.Context, r request, why *errs.Error) error {
	return m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		if decided, err := lockReceived(ctx, tx, r.FactID); err != nil || decided {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quote_request_in SET state = 'rejected', reason = $2, decided_at = now()
			WHERE id = $1`, r.FactID, why.Reason); err != nil {
			return err
		}
		le := r.LegalEntityID
		if !core.UUIDForm.MatchString(le) {
			le = ""
		}
		return core.Emit(m, ctx, tx, core.QuoteRequestRejected, r.FactID, 1, le, map[string]any{"fact_id": r.FactID,
			"opportunity_id": r.OpportunityID, "reason": why.Reason, "detail": why.Detail,
			"occurred_at": canon.Time(time.Now())})
	})
}

// lockReceived locks the request's row; decided is true when another run decided it meanwhile.
func lockReceived(ctx context.Context, tx db.Tx, id string) (decided bool, err error) {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM quote_request_in WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
		return false, err
	}
	return state != "received", nil
}
