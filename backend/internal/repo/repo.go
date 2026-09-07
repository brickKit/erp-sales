// Package repo 是 erp-sales 的数据访问层：订单头/行、价格表、客户摘要
// 副本、对账队列。本组件是阶段二唯一的链上一环——ConfirmOrder 的 TCC
// 编排（backend/internal/tcc）是这个组件真正的难点，这一层只管数据。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ── 哨兵错误。grpc/http 两层通过 service.ToStatus 统一映射（同
// erp-inventory/erp-finance 的判据）──

var ErrInvalidArgument = errors.New("参数不合法")
var ErrNotFound = errors.New("not found")

// ErrOrderNotDraft：对一张不是 DRAFT 的订单做只有 DRAFT 才能做的操作
// （如 ConfirmOrder）。
var ErrOrderNotDraft = errors.New("订单不是草稿状态")

// ErrOrderTerminal：对一张已经是终态（COMPLETED/CANCELLED/CLOSED）的
// 订单做状态流转——CANCELLED 是终态，不能复活（设计计划 §2.1）。
var ErrOrderTerminal = errors.New("订单已经是终态，不能再流转")

type Repo struct {
	db     *sql.DB
	role   string
	schema string
}

func New(db *sql.DB, role, schema string) *Repo {
	return &Repo{db: db, role: role, schema: schema}
}

// ── 幂等：claim-first（同 erp-inventory/erp-finance 的判据）──
//
// CreateOrder/ConfirmOrder/CancelOrder/ShipOrder 四个写命令都用它：先
// 原子声明（INSERT ... ON CONFLICT DO NOTHING），声明成功才做真正的
// 工作——后到的并发请求会被数据库锁挂起直到先到的事务结束，不存在
// "两边都以为自己是第一次"的窗口。
func claimIdempotency(ctx context.Context, tx *sql.Tx, key, command string) (claimed bool, err error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO command_idempotency (idempotency_key, command, result_id) VALUES ($1, $2, '')
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		key, command)
	if err != nil {
		return false, fmt.Errorf("声明 command_idempotency: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func finalizeIdempotency(ctx context.Context, tx *sql.Tx, key, resultID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE command_idempotency SET result_id = $1 WHERE idempotency_key = $2`, resultID, key)
	if err != nil {
		return fmt.Errorf("落地 command_idempotency 结果: %w", err)
	}
	return nil
}

func lookupIdempotencyResult(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var resultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT result_id FROM command_idempotency WHERE idempotency_key = $1`, key).Scan(&resultID); err != nil {
		return "", fmt.Errorf("查 command_idempotency: %w", err)
	}
	return resultID, nil
}

func parseID(field, s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s 不合法：%q", ErrInvalidArgument, field, s)
	}
	return id, nil
}
