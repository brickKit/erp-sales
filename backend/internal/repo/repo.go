// Package repo 是 erp-sales 的数据访问层：订单头/行、价格表、客户摘要
// 副本、对账队列。ConfirmOrder 的补偿编排（backend/internal/tcc）是这个
// 组件真正的难点，这一层只管数据，不发起任何网络调用。
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ── 哨兵错误。grpc / http 两层通过 service.ToStatus 统一映射成状态码 ──

var ErrInvalidArgument = errors.New("参数不合法")
var ErrNotFound = errors.New("not found")

// ErrOrderNotDraft：对一张不是 DRAFT 的订单做只有 DRAFT 才能做的操作
// （如 ConfirmOrder）。
var ErrOrderNotDraft = errors.New("订单不是草稿状态")

// ErrOrderTerminal：对一张已经是终态（COMPLETED/CANCELLED/CLOSED）的
// 订单做状态流转——CANCELLED 是终态，不能复活。
var ErrOrderTerminal = errors.New("订单已经是终态，不能再流转")

// ErrForbidden：调用者对某张具体订单既不在其 org 范围内也不是 owner。
// 这张订单真实存在，调用者只是看不见，与 ErrNotFound 语义不同，不能混用。
var ErrForbidden = errors.New("无权访问该订单")

// InScope 判断这张订单是否在调用者的数据范围内——org（dept_path 前缀
// 匹配）或 owner（owner_id 精确匹配）任一命中就算在范围内。"本部门及
// 下级"与"我的订单"是同一个人可能同时具备的两种"看得到"的理由，不是
// 互斥的，所以用 OR 不是 AND（"我的订单"和"本部门及下级的订单"是销售
// 组织最基本的两个视图）。两个操作数都来自调用者的真实范围
// （besdk.ScopeOf），不能有一个取"不限"的缺省值。
//
// 没有部门的调用者，SDK 给的 scopePrefix 是哨兵 besdk.NoDeptPath：它不以
// "/" 开头，任何真实路径、空串都不以它开头，org 一侧自然落空，只剩 owner。
// "/" 是整棵部门树的显式根标记，作为普通前缀命中所有带部门的单。SDK 保证
// 两个操作数都不是空串；万一收到空串（调用方漏填），那一侧按不命中处理——
// strings.HasPrefix(x, "") 恒为真，当成"不限"就是 fail-open。
func (o *Order) InScope(scopePrefix, scopeOwner string) bool {
	org := scopePrefix != "" && strings.HasPrefix(o.DeptPath, scopePrefix)
	owner := scopeOwner != "" && o.OwnerID == scopeOwner
	return org || owner
}

// checkViewScope 是 ListOrders / StatsSummary 共用的守卫：选中的那个视图的
// 操作数（dept 视图的 ScopePrefix、mine 视图的 ScopeOwner）来自
// besdk.ScopeOf，SDK 保证它不是空串。空串只能是调用方漏填了，报参数错误，
// 不能当成"不限"（空前缀拼进 `dept_path LIKE $n || '%'` 就匹配整张表）。
func checkViewScope(viewMine bool, scopePrefix, scopeOwner string) error {
	if viewMine && scopeOwner == "" {
		return fmt.Errorf("%w: mine 视图缺 ScopeOwner", ErrInvalidArgument)
	}
	if !viewMine && scopePrefix == "" {
		return fmt.Errorf("%w: dept 视图缺 ScopePrefix", ErrInvalidArgument)
	}
	return nil
}

// checkDeptPathSnapshot 是建单快照的守卫：dept_path 要么是空串（建单人没有
// 部门，这张单只对 owner 可见），要么是以 "/" 开头的真实路径。SDK 的"无部门"
// 哨兵不以 "/" 开头，写进行里会被另一个无部门的人的 dept 视图前缀命中。
func checkDeptPathSnapshot(deptPath string) error {
	if deptPath != "" && !strings.HasPrefix(deptPath, "/") {
		return fmt.Errorf("%w: dept_path 必须为空或以 / 开头，实际 %q", ErrInvalidArgument, deptPath)
	}
	return nil
}

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
