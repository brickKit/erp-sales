package repo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// StatsInput 是仪表盘统计的入参。数据范围与 ListOrders 完全一致：ViewMine
// 为 true 时按 owner_id，否则按 dept_path 前缀，二选一，不是 OR。
type StatsInput struct {
	Period      string    // week / month / quarter；空串等于 month
	Now         time.Time // 统计截止时刻（调用方传 time.Now()，测试可固定）
	ViewMine    bool
	ScopePrefix string
	ScopeOwner  string
}

// DayStat 是 by_day 的一项。
type DayStat struct {
	Date        string // YYYY-MM-DD（UTC）
	OrderCount  int
	TotalAmount string // 当天建的、已确认订单的金额合计
}

// Stats 是 GET /stats/summary 的结果。
type Stats struct {
	Period         string
	From, To       string // 窗口首尾日期（含），YYYY-MM-DD（UTC）
	OrderCount     int
	ConfirmedCount int
	TotalAmount    string
	ByDay          []DayStat
	ByStatus       map[string]int
}

// periodDays 是各 period 覆盖的自然日数（含今天）。
var periodDays = map[string]int{"week": 7, "month": 30, "quarter": 90}

// AllStatuses 是订单的全部状态，by_status 每个都列出（没有订单的是 0）。
var AllStatuses = []string{StatusDraft, StatusConfirmed, StatusShipped, StatusCompleted,
	StatusCancelled, StatusClosed, StatusSuspended}

// confirmedStatuses 是"已确认"的订单：确认之后的正常状态。金额只算它们——
// 草稿还没确认，取消、关闭、挂起的不是成交。
const confirmedStatuses = `('CONFIRMED', 'SHIPPED', 'COMPLETED')`

// StatsSummary 统计调用者数据范围内、窗口期内建的订单（按 created_at 归日，UTC）。
func (r *Repo) StatsSummary(ctx context.Context, in StatsInput) (*Stats, error) {
	period := in.Period
	if period == "" {
		period = "month"
	}
	days, ok := periodDays[period]
	if !ok {
		return nil, fmt.Errorf("%w: period 只能是 week / month / quarter，实际 %q", ErrInvalidArgument, in.Period)
	}
	to := in.Now.UTC()
	today := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(days - 1))

	// 范围条件与 ListOrders 相同：二选一，操作数都来自调用者的真实范围。
	scopeSQL, scopeArg := "dept_path LIKE $3 || '%'", in.ScopePrefix
	if in.ViewMine {
		scopeSQL, scopeArg = "owner_id = $3", in.ScopeOwner
	}
	where := `created_at >= $1 AND created_at <= $2 AND ` + scopeSQL
	args := []any{from, to, scopeArg}

	st := &Stats{Period: period, From: from.Format("2006-01-02"), To: today.Format("2006-01-02"),
		ByStatus: make(map[string]int, len(AllStatuses))}
	for _, s := range AllStatuses {
		st.ByStatus[s] = 0
	}
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*),
			       count(*) FILTER (WHERE status IN `+confirmedStatuses+`),
			       coalesce(sum(total_amount) FILTER (WHERE status IN `+confirmedStatuses+`), 0)::numeric(18,2)::text
			FROM sales_orders WHERE `+where, args...,
		).Scan(&st.OrderCount, &st.ConfirmedCount, &st.TotalAmount); err != nil {
			return fmt.Errorf("统计 sales_orders: %w", err)
		}
		if err := scanByStatus(ctx, tx, where, args, st.ByStatus); err != nil {
			return err
		}
		byDay, err := scanByDay(ctx, tx, where, args, from, today)
		st.ByDay = byDay
		return err
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

func scanByStatus(ctx context.Context, tx *sql.Tx, where string, args []any, out map[string]int) error {
	rows, err := tx.QueryContext(ctx, `SELECT status, count(*) FROM sales_orders WHERE `+where+` GROUP BY status`, args...)
	if err != nil {
		return fmt.Errorf("按状态统计 sales_orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return err
		}
		out[status] = n
	}
	return rows.Err()
}

// scanByDay 用 generate_series 列出窗口内每一天，没有订单的日子也有一行。
func scanByDay(ctx context.Context, tx *sql.Tx, where string, args []any, from, today time.Time) ([]DayStat, error) {
	dayArgs := append(append([]any{}, args...), from.Format("2006-01-02"), today.Format("2006-01-02"))
	rows, err := tx.QueryContext(ctx, `
		WITH o AS (
			SELECT (created_at AT TIME ZONE 'UTC')::date AS d, status, total_amount
			FROM sales_orders WHERE `+where+`
		)
		SELECT to_char(days.d, 'YYYY-MM-DD'),
		       count(o.d),
		       coalesce(sum(o.total_amount) FILTER (WHERE o.status IN `+confirmedStatuses+`), 0)::numeric(18,2)::text
		FROM generate_series($4::date, $5::date, interval '1 day') AS days(d)
		LEFT JOIN o ON o.d = days.d::date
		GROUP BY days.d ORDER BY days.d`, dayArgs...)
	if err != nil {
		return nil, fmt.Errorf("按日统计 sales_orders: %w", err)
	}
	defer rows.Close()
	var out []DayStat
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Date, &d.OrderCount, &d.TotalAmount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
