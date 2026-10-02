package tcc

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"google.golang.org/grpc"

	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
)

// 同一个商机的赢单事件再来一次（核心 NATS 不重投，但 crm/opportunity 以新的
// 事件 id 再发一次时 inbox 去不了重）：第一次已经建单、确认成功，第二次要原样
// 拿回那张单，什么都不再做——尤其不能因为"订单不是草稿"开一条"转单失败"的
// 假异常待办给商机负责人。
func TestHandleOpportunityWon_同一商机再来一次_不再预留也不开假异常待办(t *testing.T) {
	db := testDB(t)
	deps := startFakeDeps(t)
	orch := New(repo.New(db, "erp_sales_rw", "erp_sales"), deps.clients(), testWarehouseID, 2*time.Second, "u_ops_test")

	oppID := fmt.Sprintf("won-replay-%d", time.Now().UnixNano())
	p := OpportunityWonPayload{
		OpportunityID: oppID, CustomerID: "C-" + oppID,
		Items:   []OpportunityWonItem{{ProductID: "P-won-replay", Quantity: "2", QuotedUnitPrice: "1.00"}},
		OwnerID: "u_won_replay_owner", DeptPath: "/1/12/", Currency: "CNY", Revision: 1,
	}
	t.Cleanup(func() { cleanupWonOrder(db, oppID, p.CustomerID) })

	orch.HandleOpportunityWon(context.Background(), p, slog.Default())
	var status string
	if err := db.QueryRow(`SELECT status FROM erp_sales.sales_orders WHERE source_opportunity_id = $1`, oppID).Scan(&status); err != nil {
		t.Fatalf("第一次赢单应建出订单：%v", err)
	}
	if status != repo.StatusConfirmed {
		t.Fatalf("第一次赢单应确认成功，实际 %s", status)
	}
	if n := deps.tasks.Load(); n != 0 {
		t.Fatalf("第一次就成功了，不该开异常待办，实际 %d 条", n)
	}

	p.Revision = 2
	orch.HandleOpportunityWon(context.Background(), p, slog.Default())
	if n := deps.tasks.Load(); n != 0 {
		t.Fatalf("同一商机再来一次不该开异常待办，实际开了 %d 条", n)
	}
	if n := deps.reserves.Load(); n != 1 {
		t.Fatalf("同一商机再来一次不该再预留，Reserve 一共调用了 %d 次", n)
	}
	var orders int
	if err := db.QueryRow(`SELECT count(*) FROM erp_sales.sales_orders WHERE source_opportunity_id = $1`, oppID).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 1 {
		t.Fatalf("同一商机只能有一张订单，实际 %d 张", orders)
	}
}

// cleanupWonOrder 删掉这个商机转出的订单及其幂等键、outbox 行与客户快照。
func cleanupWonOrder(db *sql.DB, oppID, customerID string) {
	rows, err := db.Query(`SELECT id FROM erp_sales.sales_orders WHERE source_opportunity_id = $1`, oppID)
	if err == nil {
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			_, _ = db.Exec(`DELETE FROM erp_sales.event_outbox WHERE aggregate_id = $1 AND subject LIKE 'sales.order.%'`, fmt.Sprint(id))
			_, _ = db.Exec(`DELETE FROM erp_sales.sales_order_items WHERE order_id = $1`, id)
			_, _ = db.Exec(`DELETE FROM erp_sales.sales_orders WHERE id = $1`, id)
		}
	}
	_, _ = db.Exec(`DELETE FROM erp_sales.command_idempotency WHERE idempotency_key LIKE $1`, "crm-won:"+oppID+"%")
	_, _ = db.Exec(`DELETE FROM erp_sales.customer_snapshots WHERE customer_id = $1`, customerID)
}

// fakeDeps 在测试进程里扮演 mdm/customer、mdm/product、erp/inventory 与
// infra/workflow：任何 id 都是 ACTIVE 的客户 / 产品，预留与待办都成功，并记下
// 调用次数。
type fakeDeps struct {
	addr            string
	reserves, tasks atomic.Int32
}

func startFakeDeps(t *testing.T) *fakeDeps {
	t.Helper()
	d := &fakeDeps{}
	srv := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(srv, fakeCustomerSrv{})
	productv1.RegisterProductServiceServer(srv, fakeProductSrv{})
	inventoryv1.RegisterInventoryServiceServer(srv, &fakeInventorySrv{d: d})
	workflowv1.RegisterWorkflowServiceServer(srv, &fakeWorkflowSrv{d: d})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	d.addr = lis.Addr().String()
	return d
}

func (d *fakeDeps) clients() *client.Clients {
	ep := "http://" + d.addr
	return client.New(besdk.NewConfig(map[string]string{
		"MDM_CUSTOMER_GRPC_ENDPOINT": ep, "MDM_PRODUCT_GRPC_ENDPOINT": ep,
		"ERP_INVENTORY_GRPC_ENDPOINT": ep, "INFRA_WORKFLOW_GRPC_ENDPOINT": ep,
	}))
}

type fakeCustomerSrv struct {
	customerv1.UnimplementedCustomerServiceServer
}

func (fakeCustomerSrv) BatchGet(_ context.Context, req *customerv1.BatchGetRequest) (*customerv1.BatchGetResponse, error) {
	out := &customerv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Customers = append(out.Customers, &customerv1.Customer{
			Id: id, Name: "假客户", CreditLimit: "100000.00", Version: 1,
			Status: customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type fakeProductSrv struct {
	productv1.UnimplementedProductServiceServer
}

func (fakeProductSrv) BatchGet(_ context.Context, req *productv1.BatchGetRequest) (*productv1.BatchGetResponse, error) {
	out := &productv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Products = append(out.Products, &productv1.Product{
			Id: id, Sku: "SKU-" + id, Name: "假产品", BaseUomId: "EA", Version: 1,
			Status: productv1.ProductStatus_PRODUCT_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type fakeInventorySrv struct {
	inventoryv1.UnimplementedInventoryServiceServer
	d *fakeDeps
}

func (f *fakeInventorySrv) Reserve(_ context.Context, req *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	f.d.reserves.Add(1)
	return &inventoryv1.ReserveResponse{ReservationId: "R-" + req.IdempotencyKey}, nil
}

type fakeWorkflowSrv struct {
	workflowv1.UnimplementedWorkflowServiceServer
	d *fakeDeps
}

func (f *fakeWorkflowSrv) CreateTask(_ context.Context, req *workflowv1.CreateTaskRequest) (*workflowv1.Task, error) {
	f.d.tasks.Add(1)
	return &workflowv1.Task{Id: "T-" + req.IdempotencyKey}, nil
}
