// 仓储层的守卫：dept 视图的 ScopePrefix、mine 视图的 ScopeOwner 一定来自
// besdk.ScopeOf，SDK 保证它们不是空串。收到空串只能是调用方漏填了，必须报
// 参数错误——当成"不限"，空前缀拼进 `LIKE $n || '%'` 就匹配整张表。
package repo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestListOrders_dept视图ScopePrefix留空报InvalidArgument(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	_, err := r.ListOrders(context.Background(), ListInput{PageSize: 10})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dept 视图 ScopePrefix 留空应该是 ErrInvalidArgument，实际：%v", err)
	}
}

func TestListOrders_mine视图ScopeOwner留空报InvalidArgument(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	_, err := r.ListOrders(context.Background(), ListInput{PageSize: 10, ViewMine: true})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("mine 视图 ScopeOwner 留空应该是 ErrInvalidArgument，实际：%v", err)
	}
}

func TestStatsSummary_dept视图ScopePrefix留空报InvalidArgument(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	_, err := r.StatsSummary(context.Background(), StatsInput{Now: time.Now()})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("dept 视图 ScopePrefix 留空应该是 ErrInvalidArgument，实际：%v", err)
	}
}

func TestStatsSummary_mine视图ScopeOwner留空报InvalidArgument(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	_, err := r.StatsSummary(context.Background(), StatsInput{Now: time.Now(), ViewMine: true})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("mine 视图 ScopeOwner 留空应该是 ErrInvalidArgument，实际：%v", err)
	}
}

// 建单快照的 dept_path 只能是空串或以 "/" 开头：SDK 的"无部门"哨兵一旦写进
// 行里，另一个无部门的人的 dept 视图前缀（同一个哨兵）就会命中它。
func TestCreateOrder_dept_path不以斜杠开头报InvalidArgument(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	for _, p := range []string{"!no-dept", "1/12/", "%"} {
		_, err := r.CreateOrder(context.Background(), CreateOrderInput{
			IdempotencyKey: uniqueSuffix("nodept-guard"), CustomerID: "C-guard", CustomerName: "测试客户",
			Items: testItems(), DeptPath: p, OwnerID: "u_guard",
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("dept_path=%q 应该是 ErrInvalidArgument，实际：%v", p, err)
		}
	}
	for _, p := range []string{"", "/", "/1/12/"} {
		if _, err := r.CreateOrder(context.Background(), CreateOrderInput{
			IdempotencyKey: uniqueSuffix("nodept-guard-ok"), CustomerID: "C-guard", CustomerName: "测试客户",
			Items: testItems(), DeptPath: p, OwnerID: "u_guard",
		}); err != nil {
			t.Fatalf("dept_path=%q 应该能建单，实际：%v", p, err)
		}
	}
}
