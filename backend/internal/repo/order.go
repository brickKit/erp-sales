package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

const (
	StatusDraft     = "DRAFT"
	StatusConfirmed = "CONFIRMED"
	StatusShipped   = "SHIPPED"
	StatusCompleted = "COMPLETED"
	StatusCancelled = "CANCELLED"
	StatusClosed    = "CLOSED"
	StatusSuspended = "SUSPENDED"
)

// OrderItem 是 sales_order_items 一行的视图。**全部快照，不 join**
// （设计计划 §2.2）。
type OrderItem struct {
	ProductID   string
	ProductSKU  string
	ProductName string
	UOMID       string
	Qty         string
	UnitPrice   string
	Discount    string
	TaxRate     string
	Subtotal    string
}

// Order 是 sales_orders 一行 + 其行的视图。dept_id/dept_path/owner_id
// 是创建时快照（设计计划 §1）。
type Order struct {
	ID                   string
	OrderNo              string
	CustomerID           string
	CustomerName         string
	Status               string
	Items                []OrderItem
	TotalAmount          string
	DeptID               string
	DeptPath             string
	OwnerID              string
	ReservationID        string
	CompensationAttempts int
	SuspendedReason      string
	Version              int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func getOrderTx(ctx context.Context, tx *sql.Tx, id string) (*Order, error) {
	orderID, err := parseID("id", id)
	if err != nil {
		return nil, err
	}
	var o Order
	var rawID int64
	row := tx.QueryRowContext(ctx, `
		SELECT id, order_no, customer_id, customer_name, status, total_amount,
			dept_id, dept_path, owner_id, reservation_id, compensation_attempts,
			suspended_reason, version, created_at, updated_at
		FROM sales_orders WHERE id = $1`, orderID)
	if err := row.Scan(&rawID, &o.OrderNo, &o.CustomerID, &o.CustomerName, &o.Status, &o.TotalAmount,
		&o.DeptID, &o.DeptPath, &o.OwnerID, &o.ReservationID, &o.CompensationAttempts,
		&o.SuspendedReason, &o.Version, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: order id=%s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("查 sales_orders: %w", err)
	}
	o.ID = strconv.FormatInt(rawID, 10)

	rows, err := tx.QueryContext(ctx, `
		SELECT product_id, product_sku, product_name, uom_id, qty, unit_price, discount, tax_rate, subtotal
		FROM sales_order_items WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("查 sales_order_items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it OrderItem
		if err := rows.Scan(&it.ProductID, &it.ProductSKU, &it.ProductName, &it.UOMID,
			&it.Qty, &it.UnitPrice, &it.Discount, &it.TaxRate, &it.Subtotal); err != nil {
			return nil, err
		}
		o.Items = append(o.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &o, nil
}

func (r *Repo) GetOrder(ctx context.Context, id string) (*Order, error) {
	var o *Order
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		var err error
		o, err = getOrderTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

// BatchGetOrder 是防 N+1 的唯一合法调用方式（§3.8）。只返回找到的。
func (r *Repo) BatchGetOrder(ctx context.Context, ids []string) (found []*Order, missing []string, err error) {
	for _, id := range ids {
		o, err := r.GetOrder(ctx, id)
		if err != nil {
			if err == ErrNotFound {
				missing = append(missing, id)
				continue
			}
			return nil, nil, err
		}
		found = append(found, o)
	}
	return found, missing, nil
}

// GetOrderStatus 给上游（阶段三的 crm-opportunity）防"薛定谔的超时"用
// （§4.5）。UNSPECIFIED（空字符串）就是 NOT_FOUND 的信号，不是错误。
func (r *Repo) GetOrderStatus(ctx context.Context, id string) (string, error) {
	orderID, err := parseID("id", id)
	if err != nil {
		return "", err
	}
	var status string
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT status FROM sales_orders WHERE id = $1`, orderID).Scan(&status)
		if err == sql.ErrNoRows {
			status = ""
			return nil
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("查 sales_orders: %w", err)
	}
	return status, nil
}

// ListInput 对应 ListOrdersRequest。刻意没有 offset 字段（决策 53）。
type ListInput struct {
	Cursor        string
	PageSize      int
	CustomerID    string
	StatusFilter  string
	CreatedAfter  time.Time
	CreatedBefore time.Time
	// ── 阶段三 Task 6：org/owner 两维数据范围，"我的订单"和"本部门及
	// 下级的订单"是销售组织最基本的两个视图（设计计划 §1），由调用方
	// （listOrdersHandler 的 ?view= 参数）静态选择用哪一个，本函数只
	// 负责把选中的那个拼进 WHERE——同一条 SQL 不会同时用两个（那样会把
	// "本部门"缩窄成"本部门里我自己的"，不是设计要的两个独立视图）。
	ViewMine    bool   // true："我的订单"（owner_id = ScopeOwner）；false（默认）："本部门及下级"（dept_path 前缀匹配）
	ScopePrefix string // ViewMine=false 时用，来自 besdk.ScopeOf(ctx).Prefix
	ScopeOwner  string // ViewMine=true 时用，来自 besdk.ScopeOf(ctx).Owner
}

type ListResult struct {
	Orders     []*Order
	NextCursor string
}

func (r *Repo) ListOrders(ctx context.Context, in ListInput) (*ListResult, error) {
	q := besdk.ListWindow(besdk.Query{
		From: in.CreatedAfter, To: in.CreatedBefore, Cursor: in.Cursor, Limit: in.PageSize,
	})
	var ck *cursorKey
	if q.Cursor != "" {
		decoded, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, fmt.Errorf("非法 cursor：%w", err)
		}
		ck = &decoded
	}

	var out ListResult
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		query := `SELECT id FROM sales_orders WHERE created_at >= $1 AND created_at <= $2`
		args := []any{q.From, q.To}
		if in.ViewMine {
			args = append(args, in.ScopeOwner)
			query += fmt.Sprintf(" AND owner_id = $%d", len(args))
		} else {
			// ScopePrefix 为空（部门树根节点）时 `LIKE '' || '%'` 等价于
			// `LIKE '%'`，天然匹配全部，不需要特判（§14.2.4 的既有判据）。
			args = append(args, in.ScopePrefix)
			query += fmt.Sprintf(" AND dept_path LIKE $%d || '%%'", len(args))
		}
		if in.CustomerID != "" {
			args = append(args, in.CustomerID)
			query += fmt.Sprintf(" AND customer_id = $%d", len(args))
		}
		if in.StatusFilter != "" {
			args = append(args, in.StatusFilter)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if ck != nil {
			args = append(args, ck.CreatedAt, ck.ID)
			query += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
		}
		args = append(args, q.Limit+1)
		query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("查 sales_orders: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var rawID int64
			if err := rows.Scan(&rawID); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, rawID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// ⚠️ N+1：每个 id 再查一次头 + 行。List 的默认页大小与强制时间
		// 窗口（besdk.ListWindow）把 N 卡在合理范围内（同 erp-finance
		// ListEntries 的既有判据）。
		orders := make([]*Order, 0, len(ids))
		for _, rawID := range ids {
			o, err := getOrderTx(ctx, tx, strconv.FormatInt(rawID, 10))
			if err != nil {
				return err
			}
			orders = append(orders, o)
		}

		if len(orders) > q.Limit {
			last := orders[q.Limit-1]
			lastRawID, err := strconv.ParseInt(last.ID, 10, 64)
			if err != nil {
				return err
			}
			out.NextCursor = encodeCursor(cursorKey{CreatedAt: last.CreatedAt, ID: lastRawID})
			orders = orders[:q.Limit]
		}
		out.Orders = orders
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
