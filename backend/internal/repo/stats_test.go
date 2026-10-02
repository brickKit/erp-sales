package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// GET /stats/summary 的规则：
//   - 窗口是最近 7 / 30 / 90 个自然日（UTC，含今天），period 省略时是 30 天；
//   - 只统计调用者数据范围内的订单，范围与列表完全一致（dept 前缀或 owner，二选一）；
//   - order_count 数窗口内建的全部订单；confirmed_count 与 total_amount 只算
//     已确认（CONFIRMED / SHIPPED / COMPLETED）的；金额是十进制字符串；
//   - by_day 覆盖窗口内每一天（没有订单的日子是 0），by_status 列出全部 7 个状态。

// ensureMonthPartition 保证 at 所在月份的分区存在（按 created_at 回溯插行要用）。
func ensureMonthPartition(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	start := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS sales_orders_%s PARTITION OF sales_orders FOR VALUES FROM ('%s') TO ('%s')`,
		start.Format("2006_01_02"), start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err := besdk.WithTx(context.Background(), db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		_, err := tx.Exec(stmt)
		return err
	}); err != nil {
		t.Fatalf("建分区 %s 失败：%v", start.Format("2006-01"), err)
	}
}

// insertStatOrder 直接写一行订单头（统计只读订单头），created_at 可回溯。
func insertStatOrder(t *testing.T, db *sql.DB, deptPath, owner, status, amount string, createdAt time.Time) {
	t.Helper()
	ensureMonthPartition(t, db, createdAt)
	if err := besdk.WithTx(context.Background(), db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO sales_orders (customer_id, customer_name, status, total_amount, dept_path, owner_id, created_at, updated_at)
			VALUES ('C-stats', '统计测试客户', $1, $2, $3, $4, $5, $5)`, status, amount, deptPath, owner, createdAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStatsSummary_计数金额与按日按状态分布(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	dept := "/" + uniqueSuffix("stats-dept") + "/"
	now := time.Now()
	insertStatOrder(t, db, dept, "u_a", StatusDraft, "100.00", now)
	insertStatOrder(t, db, dept, "u_a", StatusConfirmed, "200.50", now)
	insertStatOrder(t, db, dept, "u_a", StatusShipped, "300.00", now)
	insertStatOrder(t, db, dept, "u_a", StatusCancelled, "50.00", now)
	insertStatOrder(t, db, dept, "u_a", StatusCompleted, "0.25", now.Add(-48*time.Hour))

	st, err := r.StatsSummary(context.Background(), StatsInput{Now: now, ScopePrefix: dept})
	if err != nil {
		t.Fatal(err)
	}
	if st.Period != "month" || len(st.ByDay) != 30 {
		t.Fatalf("period 省略时应是 month、by_day 30 天，实际 %s / %d 天", st.Period, len(st.ByDay))
	}
	if st.OrderCount != 5 || st.ConfirmedCount != 3 || st.TotalAmount != "500.75" {
		t.Fatalf("期望 order_count=5 confirmed_count=3 total_amount=500.75，实际 %d / %d / %s",
			st.OrderCount, st.ConfirmedCount, st.TotalAmount)
	}
	today := st.ByDay[len(st.ByDay)-1]
	if today.Date != now.UTC().Format("2006-01-02") || today.OrderCount != 4 || today.TotalAmount != "500.50" {
		t.Fatalf("今天应有 4 单、已确认金额 500.50，实际 %+v", today)
	}
	twoDaysAgo := st.ByDay[len(st.ByDay)-3]
	if twoDaysAgo.OrderCount != 1 || twoDaysAgo.TotalAmount != "0.25" {
		t.Fatalf("两天前应有 1 单、金额 0.25，实际 %+v", twoDaysAgo)
	}
	if st.ByDay[0].OrderCount != 0 || st.ByDay[0].TotalAmount != "0.00" {
		t.Fatalf("没有订单的日子应是 0 / 0.00，实际 %+v", st.ByDay[0])
	}
	want := map[string]int{StatusDraft: 1, StatusConfirmed: 1, StatusShipped: 1, StatusCompleted: 1,
		StatusCancelled: 1, StatusClosed: 0, StatusSuspended: 0}
	for k, v := range want {
		n, ok := st.ByStatus[k]
		if !ok || n != v {
			t.Fatalf("by_status[%s] 期望 %d，实际 %d（存在=%v）", k, v, n, ok)
		}
	}
}

// 有数据范围的接口必须证明"别人的数据看不到"：两个部门、两个人各自只数自己的。
func TestStatsSummary_两个范围的用户各自只看到自己的数(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	deptA := "/" + uniqueSuffix("stats-a") + "/"
	deptB := "/" + uniqueSuffix("stats-b") + "/"
	ownerA, ownerB := uniqueSuffix("u_a"), uniqueSuffix("u_b")
	now := time.Now()
	insertStatOrder(t, db, deptA, ownerA, StatusConfirmed, "10.00", now)
	insertStatOrder(t, db, deptA, ownerA, StatusConfirmed, "20.00", now)
	insertStatOrder(t, db, deptB, ownerB, StatusConfirmed, "7.00", now)
	// B 部门里属于 A 的一单：按部门看归 B，按 owner 看归 A
	insertStatOrder(t, db, deptB, ownerA, StatusConfirmed, "1.00", now)

	cases := []struct {
		name   string
		in     StatsInput
		count  int
		amount string
	}{
		{"A 部门", StatsInput{ScopePrefix: deptA}, 2, "30.00"},
		{"B 部门", StatsInput{ScopePrefix: deptB}, 2, "8.00"},
		{"A 本人", StatsInput{ViewMine: true, ScopeOwner: ownerA}, 3, "31.00"},
		{"B 本人", StatsInput{ViewMine: true, ScopeOwner: ownerB}, 1, "7.00"},
	}
	for _, c := range cases {
		c.in.Now = now
		st, err := r.StatsSummary(context.Background(), c.in)
		if err != nil {
			t.Fatal(err)
		}
		if st.OrderCount != c.count || st.TotalAmount != c.amount {
			t.Fatalf("%s：期望 %d 单 / %s，实际 %d 单 / %s", c.name, c.count, c.amount, st.OrderCount, st.TotalAmount)
		}
	}
}

func TestStatsSummary_period窗口边界(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	dept := "/" + uniqueSuffix("stats-window") + "/"
	now := time.Now()
	day := 24 * time.Hour
	insertStatOrder(t, db, dept, "u_w", StatusConfirmed, "1.00", now.Add(-6*day))  // 一周窗口的第一天
	insertStatOrder(t, db, dept, "u_w", StatusConfirmed, "2.00", now.Add(-7*day))  // 一周之外
	insertStatOrder(t, db, dept, "u_w", StatusConfirmed, "4.00", now.Add(-29*day)) // 30 天窗口的第一天
	insertStatOrder(t, db, dept, "u_w", StatusConfirmed, "8.00", now.Add(-30*day)) // 30 天之外

	cases := []struct {
		period string
		days   int
		count  int
		amount string
	}{
		{"week", 7, 1, "1.00"},
		{"month", 30, 3, "7.00"},
		{"quarter", 90, 4, "15.00"},
	}
	for _, c := range cases {
		st, err := r.StatsSummary(context.Background(), StatsInput{Period: c.period, Now: now, ScopePrefix: dept})
		if err != nil {
			t.Fatal(err)
		}
		if len(st.ByDay) != c.days || st.OrderCount != c.count || st.TotalAmount != c.amount {
			t.Fatalf("period=%s：期望 %d 天 / %d 单 / %s，实际 %d 天 / %d 单 / %s",
				c.period, c.days, c.count, c.amount, len(st.ByDay), st.OrderCount, st.TotalAmount)
		}
		wantFrom := now.UTC().AddDate(0, 0, -(c.days - 1)).Format("2006-01-02")
		if st.From != wantFrom || st.To != now.UTC().Format("2006-01-02") {
			t.Fatalf("period=%s：窗口应是 %s ~ 今天，实际 %s ~ %s", c.period, wantFrom, st.From, st.To)
		}
	}

	if _, err := r.StatsSummary(context.Background(), StatsInput{Period: "year", Now: now, ScopePrefix: dept}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("不认识的 period 应是参数错误，实际 %v", err)
	}
}
