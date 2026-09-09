package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// ExceptionReasonCompensationFailed 是 IncrementCompensationAttemptsTx 转
// SUSPENDED 时写的固定 reason 文案——ResumeFromExceptionTx 用它做精确
// 匹配，防止一张后来又被权威额度判定（finance.credit.rejected.v1）覆盖
// 过 reason 的 SUSPENDED 订单被"补偿异常已处理"事件误恢复：两条
// SUSPENDED 来源共用同一个 status 值但语义不同，reason 是唯一的区分
// 依据（设计计划 §4.4.4）。
const ExceptionReasonCompensationFailed = "补偿连续失败 3 次，需要人工介入"

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
		if err := SuspendOrderTx(ctx, tx, orderID, ExceptionReasonCompensationFailed); err != nil {
			return attempts, err
		}
	}
	return attempts, nil
}

// ResumeFromExceptionTx 把订单从 SUSPENDED 恢复回 DRAFT、清零补偿失败
// 计数——由消费 infra.workflow.task.completed.v1 的 consumer 调用（设计
// 计划 §4.4.4，阶段三 Task 8 落地）。⚠️ 只有当前 reason 精确等于
// ExceptionReasonCompensationFailed 才恢复，见该常量注释；不匹配时不
// 报错，返回 resumed=false 交给调用方决定要不要记一条排障日志（同
// SuspendOrderTx 的既有判据：终态/不匹配的情形是设计的边界，不是错误）。
//
// ⚠️ 恢复目标是 DRAFT 而不是直接当作"已确认"——FinalizeConfirm 从未
// 提交成功过（正是它失败才触发的补偿），订单本来就没有一个可恢复的
// "已确认"状态可言，DRAFT 是 ConfirmOrder 本身要求的起点，恢复到这里
// 就能被重新调用，不需要发明新状态。
func ResumeFromExceptionTx(ctx context.Context, tx *sql.Tx, orderID int64) (resumed bool, err error) {
	var status, reason string
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT status, suspended_reason FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID,
	).Scan(&status, &reason); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return false, fmt.Errorf("%w: order id=%d", ErrNotFound, orderID)
		}
		return false, fmt.Errorf("查 sales_orders: %w", scanErr)
	}
	if status != StatusSuspended || reason != ExceptionReasonCompensationFailed {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sales_orders
		SET status = $1, compensation_attempts = 0, suspended_reason = '', version = version + 1, updated_at = now()
		WHERE id = $2`, StatusDraft, orderID); err != nil {
		return false, fmt.Errorf("更新 sales_orders: %w", err)
	}
	return true, nil
}
