package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/brickKit/be-sdk-go/v3/canon"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/errs"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/idem"
	"github.com/brickKit/be-sdk-go/v3/ids"
	"github.com/jackc/pgx/v5"
)

// The order's processes over its reservation branches (protocol 04; docs/flows/confirmation.md): the confirmation,
// the cancellation of a confirmed order and the release when the order completes. advance is the one function that
// moves a process; the request's synchronous path and the recovery work item both call it.

const jobAdvance = "advance-process"

// Branch states: what a participant answered last; unknown until it answered.
const (
	bUnknown   = "unknown"
	bHeld      = "held"
	bCommitted = "committed"
)

// ended are the branch states nothing is held in any more.
var ended = map[string]bool{"released": true, "expired": true, "rejected": true, "cancelled": true, "consumed": true}

// errUnknown is a step whose outcome is not known yet: the work item retries it.
var errUnknown = errors.New("a participant's answer is not known yet")

type branch struct {
	Component string  `json:"component"`
	BranchID  string  `json:"branch_id"`
	State     string  `json:"state"`
	Reason    *string `json:"-"`
}

type process struct {
	ID        string   `json:"id"`
	OrderID   string   `json:"order_id"`
	State     string   `json:"state"`
	Kind      string   `json:"-"`
	Reason    *string  `json:"reason"`
	Revision  int      `json:"-"`
	Actor     string   `json:"-"`
	Branches  []branch `json:"participants"`
	StartedAt string   `json:"started_at"`
	UpdatedAt string   `json:"updated_at"`
	young     bool     // started within the synchronous budget
}

func terminal(state string) bool {
	return state == "confirmed" || state == "rejected" || state == "cancelled"
}

func getProcess(ctx context.Context, tx db.Tx, id string, lock bool) (process, error) {
	q := `SELECT id::text, order_id::text, state, kind, reason, revision, actor, started_at, updated_at,
		started_at > now() - make_interval(secs => $2) FROM confirmation WHERE id = $1::uuid`
	if lock {
		q += ` FOR UPDATE`
	}
	var p process
	var started, updated time.Time
	err := tx.QueryRow(ctx, q, id, SyncBudget.Seconds()).Scan(&p.ID, &p.OrderID, &p.State, &p.Kind, &p.Reason, &p.Revision,
		&p.Actor, &started, &updated, &p.young)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, errNone
		}
		return p, err
	}
	p.StartedAt, p.UpdatedAt = canon.Time(started), canon.Time(updated)
	rows, err := tx.Query(ctx, `SELECT component, branch_id::text, state, reason FROM confirmation_branch
		WHERE confirmation_id = $1 ORDER BY component`, id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	p.Branches = []branch{}
	for rows.Next() {
		var b branch
		if err := rows.Scan(&b.Component, &b.BranchID, &b.State, &b.Reason); err != nil {
			return p, err
		}
		p.Branches = append(p.Branches, b)
	}
	return p, rows.Err()
}

func (m *Module) process(ctx context.Context, id string) (process, error) {
	var p process
	err := m.rt.DB.BackgroundTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		p, err = getProcess(ctx, tx, id, false)
		return err
	})
	return p, err
}

// --- confirming an order ---

// confirmAnswer is ConfirmAnswer of the user surface.
type confirmAnswer struct {
	Outcome        string  `json:"outcome"`
	ConfirmationID string  `json:"confirmation_id"`
	Reason         *string `json:"reason"`
	Order          Order   `json:"order"`
}

// errStale asks for the evidence again: the order changed, or the evidence is older than W.
var errStale = errors.New("the evidence is stale")

// confirmOrder starts the order's confirmation and follows it for the synchronous budget (RS07); the same key again
// answers the same process (RS08).
func (m *Module) confirmOrder(c *httpx.Context) error {
	req, err := idem.FromHTTP(c)
	if err != nil {
		return err
	}
	cur, err := m.order(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	ctx, actor, err := check(c, OpOrderConfirm, docFacts(cur.LegalEntityID, cur.DepartmentPath, cur.OwnerID))
	if err != nil {
		return err
	}
	var out idem.Outcome
	var replayed bool
	for attempt := 0; ; attempt++ {
		out, replayed, err = m.startConfirmation(ctx, req, actor, cur)
		if !errors.Is(err, errStale) || attempt == 2 {
			break
		}
		if cur, err = m.order(ctx, cur.ID); err != nil {
			return err
		}
	}
	if errors.Is(err, errStale) {
		return errVersion.New("the order kept changing during its confirmation; try again")
	}
	if err != nil {
		return err
	}
	id := resultID(out)
	if !replayed {
		sync, cancel := context.WithTimeout(ctx, SyncBudget)
		_ = m.advance(sync, id) // what is left is the work item's
		cancel()
	}
	p, err := m.process(ctx, id)
	if err != nil {
		return err
	}
	o, err := m.order(ctx, p.OrderID)
	if err != nil {
		return err
	}
	a := confirmAnswer{Outcome: "processing", ConfirmationID: p.ID, Order: o}
	switch p.State {
	case "confirmed":
		a.Outcome = "confirmed"
	case "rejected", "cancelled":
		a.Outcome, a.Reason = "rejected", p.Reason
	}
	return c.JSON(200, a)
}

// startConfirmation gathers the evidence outside a transaction, then in T1 checks the order did not change, writes
// the rate snapshot and creates the process and its branch IDs; the order becomes confirming (RS09).
func (m *Module) startConfirmation(ctx context.Context, req idem.Request, actor string, o Order) (idem.Outcome, bool, error) {
	var rate *rate
	var participants []string
	if o.State == "draft" {
		asked := time.Now()
		le, err := m.legalEntityOf(ctx, o.LegalEntityID)
		if err != nil {
			return idem.Outcome{}, false, err
		}
		if _, err := m.customerOf(ctx, o.Customer.PartnerID, o.LegalEntityID); err != nil {
			return idem.Outcome{}, false, err
		}
		pids := make([]string, len(o.Lines))
		for i, l := range o.Lines {
			pids[i] = l.Product.ID
		}
		ps, err := m.productsOf(ctx, pids, o.LegalEntityID)
		if err != nil {
			return idem.Outcome{}, false, err
		}
		goods := false
		for _, l := range o.Lines {
			if ps[l.Product.ID].Stocked != (l.Kind == "goods") {
				return idem.Outcome{}, false, errIneligible.New(fmt.Sprintf("line %d: product %s changed whether it is "+
					"stocked; enter the line again", l.LineNo, l.Product.Code))
			}
			goods = goods || l.Kind == "goods"
		}
		if goods {
			if _, ok := m.rt.GRPC(Inventory); !ok {
				return idem.Outcome{}, false, errs.New(errs.ReasonDependencyUnavailable,
					"erp/inventory is not installed: an order with goods cannot be confirmed")
			}
			participants = append(participants, Inventory)
		}
		if _, ok := m.rt.GRPC(Credit); ok && o.Totals.Gross != "0" {
			participants = append(participants, Credit)
		}
		if o.Currency != le.BaseCurrency {
			r, err := m.spotRate(ctx, o.Currency, le.BaseCurrency, o.BusinessDate)
			if err != nil {
				return idem.Outcome{}, false, err
			}
			rate = &r
		}
		if time.Since(asked) > EvidenceW {
			return idem.Outcome{}, false, errStale
		}
	}
	return idem.Do(ctx, m.rt.DB, req, func(ctx context.Context, tx db.Tx) (idem.Outcome, error) {
		cur, err := getOrder(ctx, tx, o.ID, true)
		if err != nil {
			return idem.Outcome{}, err
		}
		switch {
		case cur.State == "confirming":
			return idem.Outcome{}, errState.New("the order is being confirmed (confirmation " + deref(cur.ConfirmationID) + ")")
		case cur.State != "draft":
			return idem.Outcome{}, errState.New("only a draft order is confirmed; this one is " + cur.State)
		case o.State != "draft" || cur.Version != o.Version:
			return idem.Outcome{}, errStale
		}
		if rate != nil {
			if _, err := tx.Exec(ctx, `UPDATE sales_order SET rate = $2::numeric, rate_date = $3::date, rate_type = $4,
				rate_record_id = $5, rate_record_version = $6 WHERE id = $1`, o.ID, rate.Rate, rate.Date, rate.Type,
				rate.RecordID, rate.RecordVersion); err != nil {
				return idem.Outcome{}, err
			}
		}
		id := ids.New().String()
		if _, err := tx.Exec(ctx, `INSERT INTO confirmation (id, order_id, revision, state, actor)
			VALUES ($1, $2, $3, 'reserving', $4)`, id, o.ID, cur.Revision, actor); err != nil {
			return idem.Outcome{}, err
		}
		for _, p := range participants {
			if _, err := tx.Exec(ctx, `INSERT INTO confirmation_branch (confirmation_id, component, branch_id)
				VALUES ($1, $2, $3)`, id, p, ids.New().String()); err != nil {
				return idem.Outcome{}, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sales_order SET state = 'confirming', confirmation_id = $2, reason = NULL,
			version = version + 1, updated_at = now() WHERE id = $1`, o.ID, id); err != nil {
			return idem.Outcome{}, err
		}
		if _, err := m.emitOrder(ctx, tx, o.ID); err != nil {
			return idem.Outcome{}, err
		}
		if _, err := m.rt.Work.Enqueue(ctx, tx, jobAdvance, advanceArgs{ID: id}); err != nil {
			return idem.Outcome{}, err
		}
		return idResult(id), nil
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// getConfirmation answers a process of the order.
func (m *Module) getConfirmation(c *httpx.Context) error {
	o, err := m.order(c.Ctx(), c.Param("id"))
	if err != nil {
		return err
	}
	if _, _, err := check(c, OpOrderRead, docFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID)); err != nil {
		return err
	}
	id := c.Param("confirmation_id")
	if !uuidForm.MatchString(id) {
		return notFound("confirmation")
	}
	p, err := m.process(c.Ctx(), id)
	if errors.Is(err, errNone) || err == nil && p.OrderID != o.ID {
		return notFound("confirmation")
	}
	if err != nil {
		return err
	}
	return c.JSON(200, p)
}
