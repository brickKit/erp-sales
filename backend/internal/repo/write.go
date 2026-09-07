// 四个写命令的本地落库部分：CreateOrder（无需 TCC，纯本地）+
// FinalizeConfirm/FinalizeCancel/FinalizeShip（TCC 编排里"确定要写"之后
// 那一步，backend/internal/tcc 负责在调这些函数之前把跨组件校验/远程
// 调用做完——这一层只管幂等 + 状态机 + 发事件，不发起任何网络调用）。
package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// ── CreateOrder：建 DRAFT，不预留库存（设计计划 §3）。跨组件校验
// （BatchGet 客户/产品）与定价已经由调用方（tcc 包）做完，这里只管落库 ──

type CreateOrderItemInput struct {
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

type CreateOrderInput struct {
	IdempotencyKey string
	CustomerID     string
	CustomerName   string // 快照，调用方从 mdm-customer BatchGet 结果里取
	Items          []CreateOrderItemInput
	// dept_id/dept_path/owner_id 是创建时快照（设计计划 §1）。阶段二
	// 没有真实身份链路（besdk.ScopeOf 恒返回不限，同 §14.2.3 阶段二占位），
	// 调用方目前传空字符串——列已经建好，阶段三 infra-authz 上线后回来
	// 从 JWT claims 里取真实值，不需要再改表结构。
	DeptID   string
	DeptPath string
	OwnerID  string
}

func sumSubtotals(items []CreateOrderItemInput) (string, error) {
	var total float64
	for _, it := range items {
		v, err := strconv.ParseFloat(it.Subtotal, 64)
		if err != nil {
			return "", fmt.Errorf("%w: subtotal 不是合法数字：%q", ErrInvalidArgument, it.Subtotal)
		}
		total += v
	}
	return strconv.FormatFloat(total, 'f', 2, 64), nil
}

func (r *Repo) CreateOrder(ctx context.Context, in CreateOrderInput) (*Order, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.CustomerID == "" {
		return nil, fmt.Errorf("%w: customer_id 不能为空", ErrInvalidArgument)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	total, err := sumSubtotals(in.Items)
	if err != nil {
		return nil, err
	}

	var order *Order
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "erp.sales.create")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			order, err = getOrderTx(ctx, tx, resultID)
			return err
		}

		var id int64
		var createdAt time.Time
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO sales_orders (customer_id, customer_name, status, total_amount, dept_id, dept_path, owner_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, created_at`,
			in.CustomerID, in.CustomerName, StatusDraft, total, in.DeptID, in.DeptPath, in.OwnerID,
		).Scan(&id, &createdAt); err != nil {
			return fmt.Errorf("写 sales_orders: %w", err)
		}

		// order_no 由 id 派生（"SO" + id），全局唯一，允许有缺口（设计计划
		// §9 第 3 条）——不需要额外的跨分区唯一约束。
		orderNo := "SO" + strconv.FormatInt(id, 10)
		if _, err := tx.ExecContext(ctx, `UPDATE sales_orders SET order_no = $1 WHERE id = $2`, orderNo, id); err != nil {
			return fmt.Errorf("回填 order_no: %w", err)
		}

		for _, it := range in.Items {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO sales_order_items
					(order_id, order_created_at, product_id, product_sku, product_name, uom_id,
					 qty, unit_price, discount, tax_rate, subtotal)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				id, createdAt, it.ProductID, it.ProductSKU, it.ProductName, it.UOMID,
				it.Qty, it.UnitPrice, it.Discount, it.TaxRate, it.Subtotal,
			); err != nil {
				return fmt.Errorf("写 sales_order_items product_id=%s: %w", it.ProductID, err)
			}
		}

		idStr := strconv.FormatInt(id, 10)
		if err := finalizeIdempotency(ctx, tx, in.IdempotencyKey, idStr); err != nil {
			return err
		}
		order, err = getOrderTx(ctx, tx, idStr)
		return err
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// ── FinalizeConfirm：ConfirmOrder 的第⑤步（设计计划 §3.1）。跨组件
// 校验、本地信用额度预判、Reserve 库存全部由 tcc 包在调用这个函数之前
// 做完并成功——这里只做"锁行重新确认还是 DRAFT → 转 CONFIRMED → 记
// reservation_id → 发 sales.order.created.v1"，全部在一个事务里 ──

type FinalizeConfirmInput struct {
	IdempotencyKey string
	OrderID        string
	ReservationID  string
}

func (r *Repo) FinalizeConfirm(ctx context.Context, in FinalizeConfirmInput) (*Order, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	orderID, err := parseID("order_id", in.OrderID)
	if err != nil {
		return nil, err
	}

	var order *Order
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "erp.sales.confirm")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			order, err = getOrderTx(ctx, tx, resultID)
			return err
		}

		// ⚠️ 重新加锁确认还是 DRAFT：claim-first 只防同一个 idempotency_key
		// 的并发重复，不防"两个不同 idempotency_key 打到同一个 order_id"
		// 这种客户端误用——这里再判一次是最后一道防线（设计计划 §3.1）。
		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID,
		).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: order id=%s", ErrNotFound, in.OrderID)
			}
			return fmt.Errorf("查 sales_orders: %w", err)
		}
		if status != StatusDraft {
			return fmt.Errorf("%w: order id=%s status=%s", ErrOrderNotDraft, in.OrderID, status)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE sales_orders SET status = $1, reservation_id = $2, version = version + 1, updated_at = now()
			WHERE id = $3`, StatusConfirmed, in.ReservationID, orderID); err != nil {
			return fmt.Errorf("更新 sales_orders: %w", err)
		}

		order, err = getOrderTx(ctx, tx, in.OrderID)
		if err != nil {
			return err
		}

		payload, err := buildOrderCreatedPayload(order)
		if err != nil {
			return err
		}
		if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
			Subject: "sales.order.created.v1", AggregateID: in.OrderID, Version: order.Version, Payload: payload,
		}); err != nil {
			return err
		}

		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, in.OrderID)
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

func buildOrderCreatedPayload(o *Order) ([]byte, error) {
	items := make([]map[string]string, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, map[string]string{
			"product_id": it.ProductID, "qty": it.Qty, "subtotal": it.Subtotal,
		})
	}
	return json.Marshal(map[string]any{
		"order_id": o.ID, "order_no": o.OrderNo, "customer_id": o.CustomerID,
		"items": items, "total_amount": o.TotalAmount, "version": o.Version,
	})
}

// ── FinalizeCancel：DRAFT/CONFIRMED → CANCELLED（设计计划 §2.1：
// CANCELLED 是终态，不能复活）。tcc 包已经在调用这个函数之前把
// erp-inventory.CancelReservation（CONFIRMED 的情形才需要）做完 ──

type FinalizeCancelInput struct {
	IdempotencyKey string
	OrderID        string
	Reason         string
}

func (r *Repo) FinalizeCancel(ctx context.Context, in FinalizeCancelInput) (*Order, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	orderID, err := parseID("order_id", in.OrderID)
	if err != nil {
		return nil, err
	}

	var order *Order
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "erp.sales.cancel")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			order, err = getOrderTx(ctx, tx, resultID)
			return err
		}

		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID,
		).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: order id=%s", ErrNotFound, in.OrderID)
			}
			return fmt.Errorf("查 sales_orders: %w", err)
		}
		switch status {
		case StatusCancelled:
			// 已经是 CANCELLED：幂等，如实返回当前状态，不重复发事件
			// （同 erp-inventory CancelReservation 的"状态内省"判据）。
			order, err = getOrderTx(ctx, tx, in.OrderID)
			if err != nil {
				return err
			}
			return finalizeIdempotency(ctx, tx, in.IdempotencyKey, in.OrderID)
		case StatusDraft, StatusConfirmed:
			// 可以取消，往下走
		default:
			return fmt.Errorf("%w: order id=%s status=%s 不能取消", ErrOrderTerminal, in.OrderID, status)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE sales_orders SET status = $1, version = version + 1, updated_at = now()
			WHERE id = $2`, StatusCancelled, orderID); err != nil {
			return fmt.Errorf("更新 sales_orders: %w", err)
		}

		order, err = getOrderTx(ctx, tx, in.OrderID)
		if err != nil {
			return err
		}

		payload, err := json.Marshal(map[string]any{"order_id": in.OrderID, "reason": in.Reason})
		if err != nil {
			return err
		}
		if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
			Subject: "sales.order.cancelled.v1", AggregateID: in.OrderID, Version: order.Version, Payload: payload,
		}); err != nil {
			return err
		}

		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, in.OrderID)
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// ── FinalizeShip：CONFIRMED → SHIPPED（设计计划 §2.1）。tcc 包已经在
// 调用这个函数之前把 erp-inventory.ConfirmIssue 做完 ──

type FinalizeShipInput struct {
	IdempotencyKey string
	OrderID        string
}

func (r *Repo) FinalizeShip(ctx context.Context, in FinalizeShipInput) (*Order, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	orderID, err := parseID("order_id", in.OrderID)
	if err != nil {
		return nil, err
	}

	var order *Order
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "erp.sales.ship")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			order, err = getOrderTx(ctx, tx, resultID)
			return err
		}

		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID,
		).Scan(&status); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: order id=%s", ErrNotFound, in.OrderID)
			}
			return fmt.Errorf("查 sales_orders: %w", err)
		}
		if status != StatusConfirmed {
			return fmt.Errorf("%w: order id=%s status=%s，只有 CONFIRMED 才能发货", ErrOrderNotDraft, in.OrderID, status)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE sales_orders SET status = $1, version = version + 1, updated_at = now()
			WHERE id = $2`, StatusShipped, orderID); err != nil {
			return fmt.Errorf("更新 sales_orders: %w", err)
		}

		order, err = getOrderTx(ctx, tx, in.OrderID)
		if err != nil {
			return err
		}

		items := make([]map[string]string, 0, len(order.Items))
		for _, it := range order.Items {
			items = append(items, map[string]string{"product_id": it.ProductID, "qty": it.Qty})
		}
		payload, err := json.Marshal(map[string]any{"order_id": in.OrderID, "items": items})
		if err != nil {
			return err
		}
		if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
			Subject: "sales.order.shipped.v1", AggregateID: in.OrderID, Version: order.Version, Payload: payload,
		}); err != nil {
			return err
		}

		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, in.OrderID)
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// ── 对账队列：Reserve 超时且 GetReservationStatus 也超时时的兜底
// （设计计划 §4.5、§9 第 8 条）。不阻塞用户请求，写一行就返回 ──

func (r *Repo) EnqueueReconciliation(ctx context.Context, orderID, reservationID, reason string) error {
	return besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		oid, err := parseID("order_id", orderID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sales_order_reconciliation_queue (order_id, reservation_id, reason)
			VALUES ($1, $2, $3)`, oid, reservationID, reason)
		if err != nil {
			return fmt.Errorf("写 sales_order_reconciliation_queue: %w", err)
		}
		return nil
	})
}

// ── 本地信用额度校验（ConfirmOrder TCC 链第③步）：包一层 WithTx，供
// tcc 包在没有现成 *sql.Tx 时调用（GetCustomerSnapshotTx 本身接 tx，
// 是给 consumer 复用的，同 snapshot.go 的判据）──

func (r *Repo) GetCustomerSnapshot(ctx context.Context, customerID string) (creditLimit, creditExposure string, err error) {
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		var e error
		creditLimit, creditExposure, e = GetCustomerSnapshotTx(tx, customerID)
		return e
	})
	return creditLimit, creditExposure, err
}

// IncrementCompensationAttempts 包一层 WithTx，供 tcc 包在补偿失败时调用
// （IncrementCompensationAttemptsTx 本身接 tx，是给未来可能的事件消费路径
// 复用的，同 suspend.go 其余函数的判据）。
func (r *Repo) IncrementCompensationAttempts(ctx context.Context, orderID string) (int, error) {
	id, err := parseID("order_id", orderID)
	if err != nil {
		return 0, err
	}
	var attempts int
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		var e error
		attempts, e = IncrementCompensationAttemptsTx(ctx, tx, id)
		return e
	})
	return attempts, err
}
