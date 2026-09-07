package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// SuspendOrderTx 把订单转 SUSPENDED（设计计划 §5、§3.1）：权威额度判定
// 超限（消费 finance.credit.rejected.v1）或补偿连续失败 3 次都走这里。
// ⚠️ 供事件消费路径直接调用，接 *sql.Tx（同 snapshot.go 的判据）。
//
// 已经是终态（COMPLETED/CANCELLED/CLOSED）的订单不再转 SUSPENDED——
// 终态之后的事实不该被一个异步到达的信用判定倒着改（CANCELLED 尤其
// 不能复活/转移，设计计划 §2.1）。已经是 SUSPENDED 的再收到一次是
// 幂等的，更新 reason 但不报错。
func SuspendOrderTx(ctx context.Context, tx *sql.Tx, orderID int64, reason string) error {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT status FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&current)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: order id=%d", ErrNotFound, orderID)
	}
	if err != nil {
		return fmt.Errorf("查 sales_orders: %w", err)
	}
	switch current {
	case StatusCompleted, StatusCancelled, StatusClosed:
		return nil // 终态不可再流转，静默忽略——不是错误，是设计的边界
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sales_orders SET status = $1, suspended_reason = $2, version = version + 1, updated_at = now()
		WHERE id = $3`, StatusSuspended, reason, orderID); err != nil {
		return fmt.Errorf("更新 sales_orders: %w", err)
	}
	return nil
}

// IncrementCompensationAttemptsTx 累加补偿失败计数，连续失败 3 次转
// SUSPENDED（设计计划 §3.1、§4.4.4）。返回累加后的次数。
func IncrementCompensationAttemptsTx(ctx context.Context, tx *sql.Tx, orderID int64) (int, error) {
	var attempts int
	if err := tx.QueryRowContext(ctx, `
		UPDATE sales_orders SET compensation_attempts = compensation_attempts + 1, updated_at = now()
		WHERE id = $1 RETURNING compensation_attempts`, orderID).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("更新 sales_orders: %w", err)
	}
	if attempts >= 3 {
		if err := SuspendOrderTx(ctx, tx, orderID, "补偿连续失败 3 次，需要人工介入"); err != nil {
			return attempts, err
		}
	}
	return attempts, nil
}
