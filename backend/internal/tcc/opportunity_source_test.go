package tcc

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
)

// 赢单转订单要记住来源商机：订单的 source_opportunity_id 等于商机 id，
// sales.order.created.v1 的 outbox 行带上它；手工建单为空串。
//
// 上游（mdm/customer、mdm/product、erp/inventory）用本进程里起的真 gRPC
// 服务替身回答：这里验的是本组件的编排与落库，不是上游的业务规则（那由
// confirm_test.go 里连真实容器的测试负责）。

type stubCustomer struct {
	customerv1.UnimplementedCustomerServiceServer
}

func (stubCustomer) BatchGet(_ context.Context, in *customerv1.BatchGetRequest) (*customerv1.BatchGetResponse, error) {
	out := &customerv1.BatchGetResponse{}
	for _, id := range in.Ids {
		out.Customers = append(out.Customers, &customerv1.Customer{
			Id: id, Name: "替身客户", Status: customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type stubProduct struct {
	productv1.UnimplementedProductServiceServer
}

func (stubProduct) BatchGet(_ context.Context, in *productv1.BatchGetRequest) (*productv1.BatchGetResponse, error) {
	out := &productv1.BatchGetResponse{}
	for _, id := range in.Ids {
		out.Products = append(out.Products, &productv1.Product{
			Id: id, Sku: "SKU-" + id, Name: "替身产品", BaseUomId: "1",
			Status: productv1.ProductStatus_PRODUCT_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type stubInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
}

func (stubInventory) Reserve(_ context.Context, in *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	return &inventoryv1.ReserveResponse{ReservationId: "R-" + in.IdempotencyKey}, nil
}

// startUpstreamStubs 在 127.0.0.1 上起一个 gRPC 服务，挂三个替身，返回指向它的 Clients。
func startUpstreamStubs(t *testing.T) *client.Clients {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(gs, stubCustomer{})
	productv1.RegisterProductServiceServer(gs, stubProduct{})
	inventoryv1.RegisterInventoryServiceServer(gs, stubInventory{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	addr := "http://" + lis.Addr().String()
	return client.New(besdk.NewConfig(map[string]string{
		"MDM_CUSTOMER_GRPC_ENDPOINT":  addr,
		"MDM_PRODUCT_GRPC_ENDPOINT":   addr,
		"ERP_INVENTORY_GRPC_ENDPOINT": addr,
	}))
}

func TestHandleOpportunityWon_订单与事件记住来源商机(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	orch := New(r, startUpstreamStubs(t), testWarehouseID, 5*time.Second, "")
	ctx := context.Background()

	customerID := uniqueSuffix("C-won-src")
	seedCustomerSnapshot(t, db, customerID, "1000000.00")
	opp := uniqueSuffix("opp-won-src")

	orch.HandleOpportunityWon(ctx, OpportunityWonPayload{
		OpportunityID: opp, CustomerID: customerID, OwnerID: "u_won_src", DeptPath: "/won-src/", Currency: "CNY",
		Items: []OpportunityWonItem{{ProductID: uniqueSuffix("P-won-src"), Quantity: "2", QuotedUnitPrice: "99.00"}},
	}, slog.Default())

	var orderID, status, source string
	if err := db.QueryRowContext(ctx, `
		SELECT id::text, status, source_opportunity_id FROM erp_sales.sales_orders
		WHERE customer_id = $1`, customerID).Scan(&orderID, &status, &source); err != nil {
		t.Fatalf("赢单应建出一张订单：%v", err)
	}
	if status != repo.StatusConfirmed {
		t.Fatalf("上游都正常时赢单订单应被确认，实际 %s", status)
	}
	if source != opp {
		t.Fatalf("订单的 source_opportunity_id 应等于商机 id %s，实际 %q", opp, source)
	}

	var payloadSource *string
	if err := db.QueryRowContext(ctx, `
		SELECT payload->>'source_opportunity_id' FROM erp_sales.event_outbox
		WHERE subject = 'sales.order.created.v1' AND aggregate_id = $1`, orderID).Scan(&payloadSource); err != nil {
		t.Fatal(err)
	}
	if payloadSource == nil || *payloadSource != opp {
		t.Fatalf("sales.order.created.v1 的 payload 应带 source_opportunity_id=%s，实际 %v", opp, payloadSource)
	}
}

func TestCreateOrder_手工建单来源商机为空(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	orch := New(r, startUpstreamStubs(t), testWarehouseID, 5*time.Second, "")

	order, err := orch.CreateOrder(e2eTestCtx(), uniqueSuffix("manual-create"), uniqueSuffix("C-manual"),
		[]CreateOrderItemInput{{ProductID: uniqueSuffix("P-manual"), Qty: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if order.SourceOpportunityID != "" {
		t.Fatalf("手工建单的 source_opportunity_id 应为空串，实际 %q", order.SourceOpportunityID)
	}
}
