package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	core "github.com/brickKit/erp-sales/v3/internal/core"

	sdkauthz "github.com/brickKit/be-sdk-go/v3/authz"
	"github.com/brickKit/be-sdk-go/v3/db"
	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/tiering"
)

// The completed and cancelled orders as a data class (assembly protocol 11, decisions A154, A155): an order with its
// lines, its confirmation processes and their branches, its deliveries and their lines, its billing claims and their
// lines; by legal entity and month of its last change; a year online after it ended, then on the warm route. The
// retention depends on the country's commercial and tax law and the customer: pending here, given at deployment.

// The class and the system operations that administer it (holds, deletion approvals, the destructions' replay): on the
// system resource, which has no dimension (an order's dimensions say nothing about who may hold an archive).
const (
	OrderClass        = "erp.sales.orders"
	OrderResidenceDay = 365
)

// Why an order's unit is not sealed yet (the blockers this module registers, P15.12).
const (
	WhyConfirmationOpen = "confirmation_open" // a confirmation, cancellation or release process still running
	WhyClaimOpen        = "billing_claim_open"
	WhyDeliveryIssuing  = "delivery_issuing"
)

func cols(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

const (
	tUUID, tText, tNum, tInt4, tInt8 = tiering.TypeUUID, tiering.TypeText, tiering.TypeNumeric, tiering.TypeInt4, tiering.TypeInt8
	tDate, tTime                     = tiering.TypeDate, tiering.TypeTimestamptz
)

// OrdersClass is the class: every column of every table (the SDK checks them against the database at start).
func OrdersClass() tiering.Class {
	return tiering.Class{Name: OrderClass, Category: tiering.CategoryCompleted,
		Root: tiering.Table{Name: "sales_order", PK: "id", Columns: cols("id", tUUID, "number", tText, "legal_entity_id", tUUID,
			"customer_id", tUUID, "customer_name", tText, "customer_tax_id", tText, "currency", tText, "business_date", tDate,
			"rate", tNum, "rate_date", tDate, "rate_type", tText, "rate_record_id", tUUID, "rate_record_version", tInt8,
			"owner_id", tText, "department_id", tUUID, "department_path", tText, "state", tText, "revision", tInt4,
			"quotation_id", tUUID, "confirmation_id", tUUID, "reason", tText, "net", tNum, "tax", tNum, "search", tText,
			"search_version", tInt4, "version", tInt8, "created_at", tTime, "updated_at", tTime)},
		Children: []tiering.Child{
			{Table: tiering.Table{Name: "order_line", PK: "id", Columns: cols("id", tUUID, "order_id", tUUID, "line_no", tInt4,
				"kind", tText, "product_id", tUUID, "product_code", tText, "product_name", tText, "base_unit", tText,
				"quantity", tNum, "unit", tText, "conversion_factor", tNum, "base_quantity", tNum, "warehouse_id", tUUID,
				"unit_price", tNum, "price_source", tText, "price_list_item_id", tUUID, "tax_rate", tNum, "net", tNum,
				"tax", tNum, "delivered", tNum, "billable_qty", tNum, "billable_amt", tNum, "claimed_qty", tNum,
				"claimed_amt", tNum, "billed_qty", tNum, "billed_amt", tNum)}, FK: "order_id"},
			{Table: tiering.Table{Name: "confirmation", PK: "id", Columns: cols("id", tUUID, "order_id", tUUID, "revision", tInt4,
				"state", tText, "kind", tText, "reason", tText, "actor", tText, "started_at", tTime, "updated_at", tTime)},
				FK: "order_id"},
			{Table: tiering.Table{Name: "confirmation_branch", Key: []string{"confirmation_id", "component"},
				Columns: cols("confirmation_id", tUUID, "component", tText, "branch_id", tUUID, "state", tText, "reason", tText)},
				FK: "confirmation_id", Parent: "confirmation"},
			{Table: tiering.Table{Name: "delivery", PK: "id", Columns: cols("id", tUUID, "number", tText, "order_id", tUUID,
				"legal_entity_id", tUUID, "owner_id", tText, "department_id", tUUID, "department_path", tText, "state", tText,
				"reason", tText, "business_date", tDate, "version", tInt8, "created_at", tTime, "updated_at", tTime)},
				FK: "order_id"},
			{Table: tiering.Table{Name: "delivery_line", Key: []string{"delivery_id", "order_line_id"}, Columns: cols(
				"delivery_id", tUUID, "order_line_id", tUUID, "product_id", tUUID, "warehouse_id", tUUID, "quantity", tNum,
				"base_quantity", tNum, "batch", tText, "serials", tiering.TypeTextArray)}, FK: "delivery_id", Parent: "delivery"},
			{Table: tiering.Table{Name: "billing_claim", PK: "id", Columns: cols("id", tUUID, "order_id", tUUID, "state", tText,
				"caller", tText, "expires_at", tTime, "created_at", tTime, "updated_at", tTime)}, FK: "order_id"},
			{Table: tiering.Table{Name: "billing_claim_line", Key: []string{"claim_id", "order_line_id"}, Columns: cols(
				"claim_id", tUUID, "order_line_id", tUUID, "quantity", tNum, "amount", tNum, "billed_qty", tNum,
				"billed_amt", tNum)}, FK: "claim_id", Parent: "billing_claim"}},
		Group: []string{"legal_entity_id"}, Anchor: "updated_at", Created: "created_at",
		Terminal: "r.state IN ('completed', 'cancelled')", ResidenceDays: OrderResidenceDay,
		Retention: tiering.Retention{Pending: true}, Locate: []string{"legal_entity_id", "owner_id", "department_id",
			"department_path"}, Routes: []string{tiering.RoutePostgres}, SchemaVersion: "1", HoldOperation: core.OpOrderHold,
		ApproveOperation: core.OpOrderDestroy}
}

// Tiering registers what keeps an ended order online (P15.12): a process still running on its reservations, a billing
// claim not settled, a delivery still issuing.
func Tiering(m *core.Base, r *tiering.Registry) error {
	r.Block(OrderClass, func(ctx context.Context, tx db.Tx, _ string, roots []string) (map[string][]string, error) {
		out := map[string][]string{}
		for why, q := range map[string]string{
			WhyConfirmationOpen: `SELECT DISTINCT order_id::text FROM confirmation WHERE order_id::text = ANY($1)
				AND state IN ('reserving', 'committing', 'rejecting', 'cancelling')`,
			WhyClaimOpen: `SELECT DISTINCT order_id::text FROM billing_claim WHERE order_id::text = ANY($1)
				AND state IN ('pending', 'confirmed')`,
			WhyDeliveryIssuing: `SELECT DISTINCT order_id::text FROM delivery WHERE order_id::text = ANY($1) AND state = 'issuing'`,
		} {
			rows, err := tx.Query(ctx, q, roots)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return nil, err
				}
				out[id] = append(out[id], why)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		for id := range out {
			sort.Strings(out[id])
		}
		return out, nil
	})
	return nil
}

// archivedOrder makes an archived aggregate the order the user surface answers: the same columns read by the same
// query, over the canonical rows instead of the tables (a CTE of the lines' name stands in for the lines table).
func archivedOrder(m *core.Base, ctx context.Context, a tiering.Aggregate) (core.Order, error) {
	root, err := json.Marshal(a.Root)
	if err != nil {
		return core.Order{}, err
	}
	lines, err := json.Marshal(append([]map[string]any{}, a.Children["order_line"]...))
	if err != nil {
		return core.Order{}, err
	}
	var o core.Order
	err = m.RT.DB.Tx(ctx, func(ctx context.Context, tx db.Tx) error {
		o, err = core.ScanOrder(tx.QueryRow(ctx, `WITH order_line AS (SELECT * FROM jsonb_populate_recordset(NULL::order_line, $2::jsonb))
			SELECT `+core.OrderCols+` FROM jsonb_populate_record(NULL::sales_order, $1::jsonb) o`, root, lines))
		return err
	})
	return o, err
}

// archived reads an order that left the online tables by its key (P15.22); found=false: it is not archived.
func archived(m *core.Base, ctx context.Context, id string) (core.Order, bool, error) {
	if m.RT.Tiering == nil {
		return core.Order{}, false, nil
	}
	a, found, err := m.RT.Tiering.Get(ctx, OrderClass, id)
	if err != nil || !found {
		return core.Order{}, false, err
	}
	o, err := archivedOrder(m, ctx, a)
	return o, err == nil, err
}

// orderMap is an order as an output object, masked to the fields the decision lets the caller read.
func orderMap(o core.Order, fields []string) (map[string]any, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	res := core.Definitions().Resources[1] // the orders
	return sdkauthz.Mask(&res, out, fields), nil
}

// historyParams are the history's filters, the same for the online part and the archived orders.
type historyParams struct {
	State         string `json:"state,omitempty"`
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	CustomerID    string `json:"customer_id,omitempty"`
}

func (p historyParams) match(state, legalEntity, customer string) bool {
	return (p.State == "" || p.State == state) && (p.LegalEntityID == "" || p.LegalEntityID == legalEntity) &&
		(p.CustomerID == "" || p.CustomerID == customer)
}

// Reader reads the orders' history (P15.26): the caller's current authorization on every order, online or archived.
func Reader(m *core.Base) tiering.Reader {
	cl := OrdersClass()
	str := func(v any) string {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	return tiering.Reader{Op: core.OpOrderRead,
		Facts: func(root map[string]any) sdkauthz.Facts {
			var dept *string
			if p := str(root["department_path"]); p != "" {
				dept = &p
			}
			return core.DocFacts(str(root["legal_entity_id"]), dept, str(root["owner_id"]))
		},
		Match: func(root map[string]any, raw json.RawMessage) bool {
			var p historyParams
			_ = json.Unmarshal(raw, &p)
			return p.match(str(root["state"]), str(root["legal_entity_id"]), str(root["customer_id"]))
		},
		Output: func(a tiering.Aggregate, d sdkauthz.Decision) (map[string]any, error) {
			o, err := archivedOrder(m, context.Background(), a)
			if err != nil {
				return nil, err
			}
			return orderMap(o, d.Fields)
		},
		Online: func(ctx context.Context, tx db.Tx, s tiering.Snapshot, sub *sdkauthz.Subject, raw json.RawMessage,
			emit func(map[string]any) error) error {
			var p historyParams
			_ = json.Unmarshal(raw, &p)
			ctx, decide, err := sub.Decider(ctx, core.OpOrderRead)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT `+core.OrderCols+` FROM sales_order o WHERE ($1 = '' OR o.state = $1)
				AND ($2 = '' OR o.legal_entity_id::text = $2) AND ($3 = '' OR o.customer_id::text = $3) AND `+
				s.OnlineExclusion(cl, "o"), p.State, p.LegalEntityID, p.CustomerID)
			if err != nil {
				return err
			}
			var orders []core.Order
			for rows.Next() {
				o, err := core.ScanOrder(rows)
				if err != nil {
					rows.Close()
					return err
				}
				orders = append(orders, o)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, o := range orders {
				d := decide(core.DocFacts(o.LegalEntityID, o.DepartmentPath, o.OwnerID))
				if d.Visible != sdkauthz.True || d.Action != sdkauthz.True {
					continue
				}
				out, err := orderMap(o, d.Fields)
				if err != nil {
					return err
				}
				if err := emit(out); err != nil {
					return err
				}
			}
			return nil
		}}
}

// history answers the orders' list with history=true: every order the caller may see, online or archived, complete
// within the limit, else 202 with the export it became (P15.26, TR10).
func history(m *core.Base, c *httpx.Context) error {
	if m.RT.Tiering == nil {
		return core.Invalid("history", "this deployment keeps no archive")
	}
	p := historyParams{State: c.Query("state"), LegalEntityID: c.Query("legal_entity_id"), CustomerID: c.Query("customer_id")}
	raw, _ := json.Marshal(p)
	s, _ := sdkauthz.From(c.Ctx())
	out, pending, err := m.RT.Tiering.History(c.Ctx(), s, OrderClass, raw)
	if err != nil {
		return err
	}
	if pending != nil {
		return c.JSON(http.StatusAccepted, map[string]string{"export": pending.Export})
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]["created_at"]) > fmt.Sprint(out[j]["created_at"]) })
	return c.JSON(http.StatusOK, map[string]any{"orders": out, "next_cursor": ""})
}
