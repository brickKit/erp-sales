// 没有分部门的调用者（token 里 dept_path 为空）：org 维什么都不命中，只剩
// owner 维。dept 视图是空列表、统计是零，别人的订单看不到（单张读取答
// NotFound，与不存在一样）也动不了（命令答 Forbidden）；"/" 是
// 整棵部门树的显式根标记，看得到所有带部门的订单（看不到 dept_path 为空的）。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// noDeptCtx 是一个没有部门的调用者：新开的账号在被分到部门之前就是这样。
func noDeptCtx(sub string) context.Context {
	return besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: sub, DeptPath: ""})
}

func createScopedOrder(t *testing.T, r *repo.Repo, customerID, deptPath, owner string) *repo.Order {
	t.Helper()
	o, err := r.CreateOrder(context.Background(), repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-nodept"), CustomerID: customerID, CustomerName: "测试客户",
		Items: testItems(), DeptID: "", DeptPath: deptPath, OwnerID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestListOrders_无部门的人dept视图看不到任何订单(t *testing.T) {
	svc, r := newTestService(t)
	me := uniqueSuffix("u_nodept")
	other := uniqueSuffix("u_other")
	customer := uniqueSuffix("C-nodept-list")
	createScopedOrder(t, r, customer, "/1/12/", other)
	createScopedOrder(t, r, customer, "", other) // 别人建的、同样没有部门的单
	mine := createScopedOrder(t, r, customer, "", me)

	// 不按客户过滤：dept 视图下整张表都不该有一行落进来。
	res, err := svc.ListOrders(noDeptCtx(me), repo.ListInput{PageSize: 200})
	if err != nil {
		t.Fatalf("无部门的人 dept 视图应该是空列表而不是错误，实际：%v", err)
	}
	if len(res.Orders) != 0 {
		t.Fatalf("无部门的人 dept 视图应该看不到任何订单，实际 %d 张（第一张 dept_path=%q owner=%q）",
			len(res.Orders), res.Orders[0].DeptPath, res.Orders[0].OwnerID)
	}

	// mine 视图照常：自己建的单看得到。
	res, err = svc.ListOrders(noDeptCtx(me), repo.ListInput{PageSize: 200, ViewMine: true, CustomerID: customer})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Orders) != 1 || res.Orders[0].ID != mine.ID {
		t.Fatalf("无部门的人 mine 视图应该只看到自己的 1 张单，实际 %d 张", len(res.Orders))
	}
}

func TestStatsSummary_无部门的人dept视图为零(t *testing.T) {
	svc, r := newTestService(t)
	me := uniqueSuffix("u_nodept")
	createScopedOrder(t, r, uniqueSuffix("C-nodept-stats"), "/1/12/", uniqueSuffix("u_other"))
	createScopedOrder(t, r, uniqueSuffix("C-nodept-stats"), "", uniqueSuffix("u_other"))
	createScopedOrder(t, r, uniqueSuffix("C-nodept-stats"), "", me)

	st, err := svc.StatsSummary(noDeptCtx(me), "week", false)
	if err != nil {
		t.Fatalf("无部门的人 dept 视图的统计应该是零而不是错误，实际：%v", err)
	}
	if st.OrderCount != 0 || st.ConfirmedCount != 0 || st.TotalAmount != "0.00" {
		t.Fatalf("无部门的人 dept 视图的统计应该是 0 单 / 0.00，实际 %d 单（已确认 %d）/ %s",
			st.OrderCount, st.ConfirmedCount, st.TotalAmount)
	}
	for _, d := range st.ByDay {
		if d.OrderCount != 0 {
			t.Fatalf("无部门的人 dept 视图的 by_day 应该全是 0，%s 是 %d", d.Date, d.OrderCount)
		}
	}
	for s, n := range st.ByStatus {
		if n != 0 {
			t.Fatalf("无部门的人 dept 视图的 by_status 应该全是 0，%s 是 %d", s, n)
		}
	}

	// mine 视图照常：自己那 1 张草稿单算在 order_count 里。
	st, err = svc.StatsSummary(noDeptCtx(me), "week", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.OrderCount != 1 {
		t.Fatalf("无部门的人 mine 视图应该数到自己的 1 张单，实际 %d", st.OrderCount)
	}
}

func TestGetOrder_无部门的人看别人的订单是NotFound(t *testing.T) {
	svc, r := newTestService(t)
	me := uniqueSuffix("u_nodept")
	customer := uniqueSuffix("C-nodept-get")
	for _, dept := range []string{"/1/12/", ""} {
		o := createScopedOrder(t, r, customer, dept, uniqueSuffix("u_other"))
		if _, err := svc.GetOrder(noDeptCtx(me), o.ID); !errors.Is(err, repo.ErrNotFound) {
			t.Fatalf("无部门的人看别人的订单（dept_path=%q）应该是 ErrNotFound（与不存在一样），实际：%v", dept, err)
		}
	}

	mine := createScopedOrder(t, r, customer, "", me)
	if _, err := svc.GetOrder(noDeptCtx(me), mine.ID); err != nil {
		t.Fatalf("无部门的人看自己的订单应该放行（owner 维），实际：%v", err)
	}
}

func TestConfirmOrder_无部门的人确认别人的订单是Forbidden(t *testing.T) {
	svc, r := newTestService(t)
	me := uniqueSuffix("u_nodept")
	customer := uniqueSuffix("C-nodept-confirm")
	for _, dept := range []string{"/1/12/", ""} {
		o := createScopedOrder(t, r, customer, dept, uniqueSuffix("u_other"))
		_, err := svc.ConfirmOrder(noDeptCtx(me), o.ID, uniqueSuffix("confirm-nodept"))
		if !errors.Is(err, repo.ErrForbidden) {
			t.Fatalf("无部门的人确认别人的订单（dept_path=%q）应该是 ErrForbidden，实际：%v", dept, err)
		}
		got, err := r.GetOrder(context.Background(), o.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != repo.StatusDraft {
			t.Fatalf("被拒绝的确认不该改动订单，状态应仍是 DRAFT，实际 %s", got.Status)
		}
	}
}

func TestListOrders_根标记斜杠看得到所有有部门的订单(t *testing.T) {
	svc, r := newTestService(t)
	customer := uniqueSuffix("C-root")
	east := createScopedOrder(t, r, customer, "/1/12/", uniqueSuffix("u_east"))
	other := createScopedOrder(t, r, customer, "/7/77/", uniqueSuffix("u_far"))
	noDept := createScopedOrder(t, r, customer, "", uniqueSuffix("u_nodept"))

	root := besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: uniqueSuffix("u_root"), DeptPath: "/"})
	res, err := svc.ListOrders(root, repo.ListInput{PageSize: 200, CustomerID: customer,
		CreatedAfter: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range res.Orders {
		seen[o.ID] = true
	}
	if !seen[east.ID] || !seen[other.ID] {
		t.Fatalf("根标记 \"/\" 应该看得到所有有部门的订单，实际 /1/12/=%v /7/77/=%v", seen[east.ID], seen[other.ID])
	}
	if seen[noDept.ID] {
		t.Fatal("根标记 \"/\" 不该看到 dept_path 为空的订单（那张只对 owner 可见）")
	}

	st, err := svc.StatsSummary(root, "week", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.OrderCount < 2 {
		t.Fatalf("根标记 \"/\" 的 dept 视图统计至少应数到刚建的 2 张有部门的单，实际 %d", st.OrderCount)
	}
}
