// 阶段三 Task 6：GetOrder/ListOrders 在 service 层的数据范围强制——
// 直接用 repo 层建订单（绕开 tcc，tcc.ConfirmOrder/CancelOrder/ShipOrder
// 需要真实的 mdm-customer/mdm-product/erp-inventory gRPC 依赖，本文件
// 只测不依赖它们的 GetOrder/ListOrders 两条读路径）。
package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/tcc"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var svcSeq int64

// uniqueSuffix 同 repo 包内的同名 test helper——不同包各自一份，不值得
// 为一个测试用的字符串拼接函数抽公用模块。
func uniqueSuffix(prefix string) string {
	n := atomic.AddInt64(&svcSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestService(t *testing.T) (*Service, *repo.Repo) {
	t.Helper()
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	// tcc.New 只存配置，不发起网络调用——本文件不测 ConfirmOrder/
	// CancelOrder/ShipOrder，构造它只是为了满足 New() 的签名。
	orch := tcc.New(r, "1", time.Second)
	return New(r, orch, slog.Default()), r
}

func testItems() []repo.CreateOrderItemInput {
	return []repo.CreateOrderItemInput{
		{ProductID: "P-TEST-1", ProductSKU: "SKU-1", ProductName: "测试产品1", UOMID: "EA",
			Qty: "1", UnitPrice: "10.00", Discount: "0", TaxRate: "0", Subtotal: "10.00"},
	}
}

func TestGetOrder_范围内可以看到(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-get-inscope"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(), DeptID: "12", DeptPath: "/1/12/", OwnerID: "u_owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 用同一个部门前缀（本部门及下级）去查——应该能看到。
	authed := besdk.ContextWithClaims(ctx, besdk.Claims{Sub: "u_manager", DeptPath: "/1/12/"})
	got, err := svc.GetOrder(authed, order.ID)
	if err != nil {
		t.Fatalf("范围内应该能查到，实际：%v", err)
	}
	if got.ID != order.ID {
		t.Fatalf("查到的订单 id 不对，期望 %s，实际 %s", order.ID, got.ID)
	}
}

func TestGetOrder_范围外ErrForbidden(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-get-outscope"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(), DeptID: "12", DeptPath: "/1/12/", OwnerID: "u_owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 既不在这个部门前缀下，也不是 owner——应该 ErrForbidden，不是
	// 悄悄放行、也不是 ErrNotFound（订单真实存在）。
	authed := besdk.ContextWithClaims(ctx, besdk.Claims{Sub: "u_stranger", DeptPath: "/1/99/"})
	_, err = svc.GetOrder(authed, order.ID)
	if !errors.Is(err, repo.ErrForbidden) {
		t.Fatalf("范围外应该是 ErrForbidden，实际：%v", err)
	}
}

func TestGetOrder_owner精确命中即使部门不同(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-get-owner"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(), DeptID: "12", DeptPath: "/1/12/", OwnerID: "u_owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 部门前缀完全不搭边，但 sub 恰好是这张订单的 owner——依然该放行
	// （"我的订单"是独立于部门范围之外的第二条看得到的理由，OR 不是 AND）。
	authed := besdk.ContextWithClaims(ctx, besdk.Claims{Sub: "u_owner", DeptPath: "/9/99/"})
	got, err := svc.GetOrder(authed, order.ID)
	if err != nil {
		t.Fatalf("owner 精确命中应该放行，实际：%v", err)
	}
	if got.ID != order.ID {
		t.Fatalf("查到的订单 id 不对")
	}
}

func TestListOrders_service层注入ScopeOf的值(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-list"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(), DeptID: "12", DeptPath: "/1/12/", OwnerID: "u_owner",
	})
	if err != nil {
		t.Fatal(err)
	}

	// service.ListOrders 应该自己从 ctx 取 ScopeOf 填 ListInput，调用方
	// （http handler）不需要、也不应该自己读 Claims。
	authed := besdk.ContextWithClaims(ctx, besdk.Claims{Sub: "u_manager", DeptPath: "/1/12/"})
	res, err := svc.ListOrders(authed, repo.ListInput{PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range res.Orders {
		if o.ID == order.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListOrders 应该用 ctx 里的 dept_path 自动过滤出这张订单，实际结果：%+v", res.Orders)
	}

	// 范围外的部门——不该看到。
	stranger := besdk.ContextWithClaims(ctx, besdk.Claims{Sub: "u_stranger", DeptPath: "/9/99/"})
	res, err = svc.ListOrders(stranger, repo.ListInput{PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range res.Orders {
		if o.ID == order.ID {
			t.Fatalf("范围外的部门不该看到这张订单")
		}
	}
}
