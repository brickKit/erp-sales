package orders

import (
	"context"
	"fmt"
	"strings"

	"github.com/brickKit/be-sdk-go/v3/db"
	core "github.com/brickKit/erp-sales/v3/internal/core"
)

// The order's lifecycle in one table (docs/design.md, "订单的状态"; rules SO01-SO15). Every command on an order asks
// Decide what it may do in the order's phase, and every change of the order's state goes through Transition. A new
// state or command is a new phase or command here: TestTheLifecycleDecidesEveryCell fails until each of its cells is
// decided, and TestTheDesignDocumentIsTheLifecycle until docs/design.md says the same.

// Phase is where an order is: its stored state, with a confirmed order whose cancel process is under way told apart.
type Phase string

const (
	Draft      Phase = "draft"
	Confirming Phase = "confirming"
	Confirmed  Phase = "confirmed"
	Cancelling Phase = "cancelling" // stored as confirmed, a cancel process under way
	Completed  Phase = "completed"
	Cancelled  Phase = "cancelled"
)

// Phases are every phase, in the order of docs/design.md.
var Phases = []Phase{Draft, Confirming, Confirmed, Cancelling, Completed, Cancelled}

// Command is what happens to an order: a user's or a component's command, or a process ending.
type Command string

const (
	Edit                  Command = "edit"                   // change a draft (PATCH)
	Confirm               Command = "confirm"                // submit for confirmation
	Cancel                Command = "cancel"                 // cancel
	Deliver               Command = "deliver"                // create a delivery
	Claim                 Command = "claim"                  // a billing claim (erp/billing)
	Complete              Command = "complete"               // every line delivered and billed
	ConfirmationConfirmed Command = "confirmation_confirmed" // the confirmation process ended confirmed
	ConfirmationRejected  Command = "confirmation_rejected"  // ... ended rejected (a participant refused, a timeout)
	ConfirmationCancelled Command = "confirmation_cancelled" // ... ended cancelled (cancelled while confirming)
	CancelEnded           Command = "cancel_ended"           // the cancel process of a confirmed order ended
)

// Commands are every command, in the order of docs/design.md.
var Commands = []Command{Edit, Confirm, Cancel, Deliver, Claim, Complete, ConfirmationConfirmed, ConfirmationRejected,
	ConfirmationCancelled, CancelEnded}

// Effect is what a command does in a phase.
type Effect string

const (
	Refuse  Effect = "refuse"  // refused with Decision.Refusal
	Ignore  Effect = "ignore"  // nothing happens; the command answers as done (a repeat, a process that no longer applies)
	Proceed Effect = "proceed" // the command does its work; the order's state stays
	Move    Effect = "move"    // the command does its work and the order moves to Decision.To (Transition)
	Process Effect = "process" // the command goes through a process (Decision.Via); the order moves when it ends
)

// Decision is one cell of the table.
type Decision struct {
	Rule    string // the rule in docs/design.md
	Effect  Effect
	To      string // Move: the order's state after
	Audit   bool   // Move: the change is audited with Op (protocol 03); submitting a confirmation is not, its outcome is
	Op      string // Move: the operation the audit names
	Via     string // Process: "confirmation" (the running confirmation turns cancelling) or "cancel" (a cancel process)
	Refusal string // Refuse: the words; {state} and {confirmation} are the order's
	Guard   string // a further condition the command checks before it acts, "" none
}

const (
	notDraftEdit    = "only a draft order changes; this one is {state} (revisions come later)"
	notDraftConfirm = "only a draft order is confirmed; this one is {state}"
	notConfirmedDel = "only a confirmed order is delivered; this one is {state}"
	notBillable     = "order_not_billable" // a claim problem, not an error (protocol 05)
	// GuardNothingStarted refuses cancelling a confirmed order that has deliveries or billing.
	GuardNothingStarted = "the order has deliveries or billing: what was delivered goes back through a return; " +
		"closing what is not delivered comes later"
	// GuardCurrentConfirmation: a confirmation's end moves the order only when it is the order's current confirmation.
	GuardCurrentConfirmation = "the process is the order's current confirmation"
	// GuardEvidence: a confirmation is submitted only on the order as it was when its evidence was taken (version).
	GuardEvidence = "the order is as when its evidence was taken"
	// GuardAllDone: an order completes when every line is delivered and billed.
	GuardAllDone = "every line delivered and billed"
)

func refuse(rule, words string) Decision { return Decision{Rule: rule, Effect: Refuse, Refusal: words} }
func ignore(rule string) Decision        { return Decision{Rule: rule, Effect: Ignore} }
func move(rule, to, op string) Decision {
	return Decision{Rule: rule, Effect: Move, To: to, Audit: true, Op: op}
}

// lifecycle is the table: every phase decides every command.
var lifecycle = map[Command]map[Phase]Decision{
	Edit: {
		Draft:      {Rule: "SO02", Effect: Proceed},
		Confirming: refuse("SO02", "the order is being confirmed; it changes after the confirmation ends"),
		Confirmed:  refuse("SO02", notDraftEdit),
		Cancelling: refuse("SO02", notDraftEdit),
		Completed:  refuse("SO02", notDraftEdit),
		Cancelled:  refuse("SO02", notDraftEdit),
	},
	Confirm: {
		Draft:      {Rule: "SO03", Effect: Move, To: "confirming", Guard: GuardEvidence},
		Confirming: refuse("SO03", "the order is being confirmed (confirmation {confirmation})"),
		Confirmed:  refuse("SO03", notDraftConfirm),
		Cancelling: refuse("SO03", notDraftConfirm),
		Completed:  refuse("SO03", notDraftConfirm),
		Cancelled:  refuse("SO03", notDraftConfirm),
	},
	Cancel: {
		Draft:      move("SO07", "cancelled", core.OpOrderCancel),
		Confirming: {Rule: "SO08", Effect: Process, Via: "confirmation"},
		Confirmed:  {Rule: "SO09", Effect: Process, Via: "cancel", Guard: GuardNothingStarted},
		Cancelling: ignore("SO11"),
		Completed:  refuse("SO15", "a completed order is not cancelled"),
		Cancelled:  ignore("SO11"),
	},
	Deliver: {
		Draft:      refuse("SO12", notConfirmedDel),
		Confirming: refuse("SO12", notConfirmedDel),
		Confirmed:  {Rule: "SO12", Effect: Proceed},
		Cancelling: refuse("SO12", "the order is being cancelled"),
		Completed:  refuse("SO12", notConfirmedDel),
		Cancelled:  refuse("SO12", notConfirmedDel),
	},
	Claim: {
		Draft:      refuse("SO13", notBillable),
		Confirming: refuse("SO13", notBillable),
		Confirmed:  {Rule: "SO13", Effect: Proceed},
		Cancelling: refuse("SO13", notBillable),
		Completed:  refuse("SO13", notBillable),
		Cancelled:  refuse("SO13", notBillable),
	},
	Complete: {
		Draft:      ignore("SO14"),
		Confirming: ignore("SO14"),
		Confirmed:  {Rule: "SO14", Effect: Move, To: "completed", Audit: true, Op: core.OpSysBilling, Guard: GuardAllDone},
		Cancelling: ignore("SO14"),
		Completed:  ignore("SO14"),
		Cancelled:  ignore("SO14"),
	},
	ConfirmationConfirmed: {
		Draft:      ignore("SO04"),
		Confirming: {Rule: "SO04", Effect: Move, To: "confirmed", Audit: true, Op: core.OpOrderConfirm, Guard: GuardCurrentConfirmation},
		Confirmed:  ignore("SO04"),
		Cancelling: ignore("SO04"),
		Completed:  ignore("SO04"),
		Cancelled:  ignore("SO04"),
	},
	ConfirmationRejected: {
		Draft:      ignore("SO05"),
		Confirming: {Rule: "SO05", Effect: Move, To: "draft", Audit: true, Op: core.OpOrderConfirm, Guard: GuardCurrentConfirmation},
		Confirmed:  ignore("SO05"),
		Cancelling: ignore("SO05"),
		Completed:  ignore("SO05"),
		Cancelled:  ignore("SO05"),
	},
	ConfirmationCancelled: {
		Draft:      ignore("SO06"),
		Confirming: {Rule: "SO06", Effect: Move, To: "cancelled", Audit: true, Op: core.OpOrderCancel, Guard: GuardCurrentConfirmation},
		Confirmed:  ignore("SO06"),
		Cancelling: ignore("SO06"),
		Completed:  ignore("SO06"),
		Cancelled:  ignore("SO06"),
	},
	CancelEnded: {
		Draft:      ignore("SO10"),
		Confirming: ignore("SO10"),
		Confirmed:  move("SO10", "cancelled", core.OpOrderCancel),
		Cancelling: move("SO10", "cancelled", core.OpOrderCancel),
		Completed:  ignore("SO10"),
		Cancelled:  ignore("SO10"),
	},
}

// Decide answers what command c does in phase p.
func Decide(p Phase, c Command) Decision {
	d, ok := lifecycle[c][p]
	if !ok { // the table test keeps this from happening
		return Decision{Effect: Refuse, Refusal: fmt.Sprintf("no rule for %s in %s", c, p)}
	}
	return d
}

// PhaseOf is the order's phase; o is read in tx under the order's lock.
func PhaseOf(ctx context.Context, tx db.Tx, o core.Order) (Phase, error) {
	if o.State != string(Confirmed) {
		return Phase(o.State), nil
	}
	var cancelling bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM confirmation WHERE order_id = $1 AND kind = 'cancel'
		AND state = 'cancelling')`, o.ID).Scan(&cancelling); err != nil {
		return "", err
	}
	if cancelling {
		return Cancelling, nil
	}
	return Confirmed, nil
}

// StoredPhase is the phase without telling a cancel under way apart, for a decision outside a transaction; only for
// the commands whose cells are the same in Confirmed and Cancelling (TestStoredPhaseIsEnough).
func StoredPhase(o core.Order) Phase { return Phase(o.State) }

// StoredPhaseCommands are the commands StoredPhase may decide.
var StoredPhaseCommands = []Command{Confirm}

// Refused is the refusal of d for o, as the STATE_CONFLICT error the user surface answers.
func Refused(d Decision, o core.Order) error { return core.ErrState.New(Words(d, o)) }

// Words is d's refusal for o.
func Words(d Decision, o core.Order) string {
	confirmation := ""
	if o.ConfirmationID != nil {
		confirmation = *o.ConfirmationID
	}
	return strings.NewReplacer("{state}", o.State, "{confirmation}", confirmation).Replace(d.Refusal)
}

// Set is what a transition writes beside the state.
type Set struct {
	Reason         *string // the order's reason (a rejection's or a cancellation's); nil clears it
	ConfirmationID string  // the order's current confirmation, "" keeps it
}

// Transition is the one writer of the order's state: d is a Move decided for o, read under the order's lock in tx.
// It publishes the order's state and audits the change when the cell says so.
func Transition(m *core.Base, ctx context.Context, tx db.Tx, o core.Order, d Decision, set Set, actor string) error {
	if d.Effect != Move {
		return fmt.Errorf("orders: transition of %s on a %s decision", o.ID, d.Effect)
	}
	var confirmation *string
	if set.ConfirmationID != "" {
		confirmation = &set.ConfirmationID
	}
	tag, err := tx.Exec(ctx, `UPDATE sales_order SET state = $2, reason = $3, confirmation_id = coalesce($4::uuid, confirmation_id),
		version = version + 1, updated_at = now() WHERE id = $1 AND state = $5`, o.ID, d.To, set.Reason, confirmation, o.State)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("orders: %s is no longer %s", o.ID, o.State)
	}
	after, err := core.EmitOrder(m, ctx, tx, o.ID)
	if err != nil || !d.Audit {
		return err
	}
	return core.Change(m, ctx, tx, actor, d.Op, core.Orders, o.ID, after)
}
