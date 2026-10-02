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
// 依据。
const ExceptionReasonCompensationFailed = "补偿连续失败 3 次，需要人工介入"

// SuspendOutcome 是 SuspendOrderTx 对这张订单做了什么。
type SuspendOutcome int

const (
	// Suspended：订单现在是 SUSPENDED（刚转过去，或本来就是、reason 已更新）。
	Suspended SuspendOutcome = iota
	// SuspendIgnoredTerminal：终态（COMPLETED/CANCELLED/CLOSED），没动。
	SuspendIgnoredTerminal
	// SuspendIgnoredShipped：已经发货，没动。
	SuspendIgnoredShipped
)

// SuspendOrderTx 把订单转 SUSPENDED：权威额度判定
// 超限（消费 finance.credit.rejected.v1）或补偿连续失败 3 次都走这里。
// 供事件消费路径直接调用，接 *sql.Tx（不自己开事务，理由同 snapshot.go）。
//
// 已经是终态（COMPLETED/CANCELLED/CLOSED）的订单不再转 SUSPENDED——
// 终态之后的事实不该被一个异步到达的信用判定倒着改（CANCELLED 尤其
// 不能复活或转移）。已经发货（SHIPPED）的也不动：库存已经出库，挂起它
// 既拦不住货、也没有任何命令能从 SUSPENDED 走回发货之后的状态，只会把一张
// 已发货的单藏起来；调用方据 SuspendIgnoredShipped 去告诉人。已经是
// SUSPENDED 的再收到一次是幂等的，更新 reason 但不报错。
func SuspendOrderTx(ctx context.Context, tx *sql.Tx, orderID int64, reason string) (SuspendOutcome, error) {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT status FROM sales_orders WHERE id = $1 FOR UPDATE`, orderID).Scan(&current)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("%w: order id=%d", ErrNotFound, orderID)
	}
	if err != nil {
		return 0, fmt.Errorf("查 sales_orders: %w", err)
	}
	switch current {
	case StatusCompleted, StatusCancelled, StatusClosed:
		return SuspendIgnoredTerminal, nil // 终态不可再流转，静默忽略——不是错误，是设计的边界
	case StatusShipped:
		return SuspendIgnoredShipped, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sales_orders SET status = $1, suspended_reason = $2, version = version + 1, updated_at = now()
		WHERE id = $3`, StatusSuspended, reason, orderID); err != nil {
		return 0, fmt.Errorf("更新 sales_orders: %w", err)
	}
	return Suspended, nil
}

// SuspendedByCompensation 报告这张单是不是因补偿连续失败挂起的：确认从没提交，
// 没有 reservation_id，它占着的那份预留只有异常待办的处理人知道怎么释放，所以
// 不能直接取消（见 tcc.CancelOrder）。
func (o *Order) SuspendedByCompensation() bool {
	return o.Status == StatusSuspended && o.SuspendedReason == ExceptionReasonCompensationFailed
}

// IncrementCompensationAttemptsTx 累加补偿失败计数，连续失败 3 次转
// SUSPENDED。返回累加后的次数。
func IncrementCompensationAttemptsTx(ctx context.Context, tx *sql.Tx, orderID int64) (int, error) {
	var attempts int
	if err := tx.QueryRowContext(ctx, `
		UPDATE sales_orders SET compensation_attempts = compensation_attempts + 1, updated_at = now()
		WHERE id = $1 RETURNING compensation_attempts`, orderID).Scan(&attempts); err != nil {
		return 0, fmt.Errorf("更新 sales_orders: %w", err)
	}
	if attempts >= 3 {
		if _, err := SuspendOrderTx(ctx, tx, orderID, ExceptionReasonCompensationFailed); err != nil {
			return attempts, err
		}
	}
	return attempts, nil
}

// ResumeFromExceptionTx 把订单从 SUSPENDED 恢复回 DRAFT、清零补偿失败
// 计数——由消费 infra.workflow.task.completed.v1 的 consumer 调用（人在待办
// 里确认补偿异常已处理）。只有当前 reason 精确等于
// ExceptionReasonCompensationFailed 才恢复，见该常量注释；不匹配时不
// 报错，返回 resumed=false 交给调用方决定要不要记一条排障日志（与
// SuspendOrderTx 一样：终态 / 不匹配的情形是设计的边界，不是错误）。
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
