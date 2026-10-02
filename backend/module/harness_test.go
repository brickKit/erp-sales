package module

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// salesHarness 把本组件经 module.New 装起来，依赖（mdm/customer、mdm/product、
// erp/inventory）用测试进程里的假 gRPC 服务顶上，并数每个远程写调用了几次：
// 重放测的就是"第二次没有再调用依赖"。库是 brickkit_test_db 里的 erp_sales
// （make test-db-init 建好的那一套）。
//
// 生产里调用者身份由 SDK 的 REST 中间件从 JWT 里取；这里由服务端拦截器塞进
// ctx。出站 metadata 带 noClaimsKey 时不塞，模拟一次不带用户身份的 gRPC 调用。
// 另一个拦截器把 panic 变成 Internal，与 SDK 的 gRPC recovery 一样。
type salesHarness struct {
	t     *testing.T
	db    *sql.DB
	deps  *fakeDeps
	sales salesv1.SalesServiceClient
	sfx   string
	ids   []string // 本测试建的订单，Cleanup 时删
}

const noClaimsKey = "x-test-no-claims"

func newSalesHarness(t *testing.T) *salesHarness {
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

	deps := &fakeDeps{}
	depSrv := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(depSrv, &fakeCountingCustomers{deps: deps})
	productv1.RegisterProductServiceServer(depSrv, fakeProducts{})
	inventoryv1.RegisterInventoryServiceServer(depSrv, &fakeInventory{deps: deps})
	depAddr := serve(t, depSrv)

	rt := besdk.NewShellRuntime(besdk.ShellModuleConfig{
		ComponentID: "erp/sales", ComponentVersion: "2.0.0",
		Env: map[string]string{
			"PG_SCHEMA": "erp_sales", "PG_USER": "erp_sales_rw", "DEFAULT_WAREHOUSE_ID": "1",
			"MDM_CUSTOMER_GRPC_ENDPOINT":  "http://" + depAddr,
			"MDM_PRODUCT_GRPC_ENDPOINT":   "http://" + depAddr,
			"ERP_INVENTORY_GRPC_ENDPOINT": "http://" + depAddr,
		},
	}, db, nil)
	mod, err := New(context.Background(), rt)
	if err != nil {
		t.Fatalf("module.New：%v", err)
	}

	claims := besdk.Claims{Sub: "u_harness", DeptPath: "/harness/"}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
			defer func() {
				if p := recover(); p != nil {
					err = status.Errorf(codes.Internal, "panic: %v", p)
				}
			}()
			return h(ctx, req)
		},
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if md, _ := metadata.FromIncomingContext(ctx); len(md.Get(noClaimsKey)) > 0 {
				return h(ctx, req)
			}
			return h(besdk.ContextWithClaims(ctx, claims), req)
		},
	))
	mod.RegisterGRPC(srv)
	conn, err := grpc.NewClient(serve(t, srv), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	h := &salesHarness{t: t, db: db, deps: deps, sales: salesv1.NewSalesServiceClient(conn),
		sfx: fmt.Sprintf("%d", time.Now().UnixNano())}
	t.Cleanup(h.cleanup)
	return h
}

// key 派生本次运行唯一的幂等键：固定键从第二次运行起就只走重放，写路径根本不跑。
func (h *salesHarness) key(name string) string { return "harness-" + name + "-" + h.sfx }

// createDraft 建一张草稿单（客户也是本次运行独有的，额度快照不串）。
func (h *salesHarness) createDraft(name string) *salesv1.Order {
	h.t.Helper()
	resp, err := h.sales.CreateOrder(context.Background(), &salesv1.CreateOrderRequest{
		IdempotencyKey: h.key(name + "-create"), CustomerId: "C-harness-" + h.sfx,
		Items: []*salesv1.CreateOrderItemInput{{ProductId: "P-harness", Qty: "1"}},
	})
	if err != nil {
		h.t.Fatalf("建单失败：%v", err)
	}
	h.ids = append(h.ids, resp.Order.Id)
	return resp.Order
}

func (h *salesHarness) cleanup() {
	c := context.Background()
	for _, id := range h.ids {
		_, _ = h.db.ExecContext(c, `DELETE FROM erp_sales.event_outbox WHERE aggregate_id = $1 AND subject LIKE 'sales.order.%'`, id)
		_, _ = h.db.ExecContext(c, `DELETE FROM erp_sales.sales_order_items WHERE order_id = $1`, id)
		_, _ = h.db.ExecContext(c, `DELETE FROM erp_sales.sales_orders WHERE id = $1`, id)
	}
	_, _ = h.db.ExecContext(c, `DELETE FROM erp_sales.customer_snapshots WHERE customer_id = $1`, "C-harness-"+h.sfx)
	_, _ = h.db.ExecContext(c, `DELETE FROM erp_sales.command_idempotency WHERE idempotency_key LIKE $1`, "harness-%-"+h.sfx)
}

// fakeDeps 记下每个远程调用的次数，并可以让客户变成不可用。
type fakeDeps struct {
	customerCalls, reserves, issues, cancels atomic.Int32
	customerInactive                         atomic.Bool
}

type fakeCountingCustomers struct {
	customerv1.UnimplementedCustomerServiceServer
	deps *fakeDeps
}

func (f *fakeCountingCustomers) BatchGet(ctx context.Context, req *customerv1.BatchGetRequest) (*customerv1.BatchGetResponse, error) {
	f.deps.customerCalls.Add(1)
	resp, err := fakeCustomers{}.BatchGet(ctx, req)
	if err == nil && f.deps.customerInactive.Load() {
		for _, c := range resp.Customers {
			c.Status = customerv1.CustomerStatus_CUSTOMER_STATUS_DISABLED
		}
	}
	return resp, err
}

type fakeInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
	deps *fakeDeps
}

func (f *fakeInventory) Reserve(_ context.Context, req *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	f.deps.reserves.Add(1)
	return &inventoryv1.ReserveResponse{ReservationId: "R-" + req.IdempotencyKey}, nil
}

func (f *fakeInventory) ConfirmIssue(_ context.Context, _ *inventoryv1.ConfirmIssueRequest) (*inventoryv1.ConfirmIssueResponse, error) {
	f.deps.issues.Add(1)
	return &inventoryv1.ConfirmIssueResponse{}, nil
}

func (f *fakeInventory) CancelReservation(_ context.Context, _ *inventoryv1.CancelReservationRequest) (*inventoryv1.CancelReservationResponse, error) {
	f.deps.cancels.Add(1)
	return &inventoryv1.CancelReservationResponse{}, nil
}
