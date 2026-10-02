package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"google.golang.org/grpc"

	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/tcc"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
)

// finance.credit.rejected.v1 在 sales.order.created.v1 之后才到：订单已经是
// CONFIRMED、库存已经预留。挂起之后必须有路可走——取消要释放那份预留——而且
// 要有人知道这件事（开异常待办）；已经发货的单库存早已出库，挂起它没有意义。

const creditOwner = "u_credit_owner"

func TestCreditRejected_确认后被拒_挂起并开异常待办_取消时释放预留(t *testing.T) {
	f := newCreditFixture(t)
	order := f.confirmedOrder("suspend")

	f.consumeRejection(order.ID)

	got := f.reload(order.ID)
	if got.Status != repo.StatusSuspended {
		t.Fatalf("被财务拒绝后应 SUSPENDED，实际 %s", got.Status)
	}
	tasks := f.deps.taskRequests()
	if len(tasks) != 1 {
		t.Errorf("挂起订单应开一条异常待办，实际 %d 条", len(tasks))
	} else if task := tasks[0]; task.SourceId != order.ID || task.AssigneeSub != creditOwner ||
		task.Type != workflowv1.TaskType_TASK_TYPE_EXCEPTION || task.SourceComponent != "erp/sales" {
		t.Errorf("异常待办应指向这张单、派给订单负责人：%+v", task)
	}

	cancelled, err := f.orch.CancelOrder(f.ctx, order.ID, f.key("suspend-cancel"), "额度不够，取消")
	if err != nil {
		t.Fatalf("挂起且占着预留的订单应能取消，实际：%v", err)
	}
	if cancelled.Status != repo.StatusCancelled {
		t.Fatalf("取消后应 CANCELLED，实际 %s", cancelled.Status)
	}
	if released := f.deps.cancelledReservations(); len(released) != 1 || released[0] != order.ReservationID {
		t.Fatalf("取消应向 erp/inventory 释放这张单的预留 %s，实际释放了 %v", order.ReservationID, released)
	}
}

func TestCreditRejected_已发货的单_不挂起_记Warn并开异常待办(t *testing.T) {
	f := newCreditFixture(t)
	order := f.confirmedOrder("shipped")
	if _, err := f.orch.ShipOrder(f.ctx, order.ID, f.key("shipped-ship")); err != nil {
		t.Fatalf("发货失败：%v", err)
	}

	f.consumeRejection(order.ID)

	if got := f.reload(order.ID); got.Status != repo.StatusShipped {
		t.Fatalf("已发货的单库存已出库，不该被挂起，实际 %s", got.Status)
	}
	if n := f.logs.count(slog.LevelWarn); n != 1 {
		t.Fatalf("应记一条 Warn，实际 %d 条", n)
	}
	tasks := f.deps.taskRequests()
	if len(tasks) != 1 || tasks[0].SourceId != order.ID || tasks[0].AssigneeSub != creditOwner {
		t.Fatalf("已发货的单被拒应开一条指向它的异常待办，实际 %+v", tasks)
	}
}

// 补偿连续失败挂起的单没有 reservation_id（确认从没提交），它的预留只有异常待办的
// 处理人知道怎么释放：直接取消会让那份预留永远没人管。
func TestCreditRejected_补偿挂起的单仍不能直接取消(t *testing.T) {
	f := newCreditFixture(t)
	order := f.draftOrder("compensation")
	for i := 0; i < 3; i++ {
		if _, err := f.repo.IncrementCompensationAttempts(f.ctx, order.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.reload(order.ID); got.Status != repo.StatusSuspended {
		t.Fatalf("前提：补偿失败 3 次应 SUSPENDED，实际 %s", got.Status)
	}

	if _, err := f.orch.CancelOrder(f.ctx, order.ID, f.key("compensation-cancel"), "试试"); err == nil {
		t.Fatal("补偿挂起的单不该能直接取消")
	}
	if got := f.reload(order.ID); got.Status != repo.StatusSuspended {
		t.Fatalf("拒绝取消后订单应仍是 SUSPENDED，实际 %s", got.Status)
	}
	if released := f.deps.cancelledReservations(); len(released) != 0 {
		t.Fatalf("不该释放任何预留，实际 %v", released)
	}
}

// ── 夹具 ──

type creditFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *sql.DB
	repo *repo.Repo
	orch *tcc.Orchestrator
	deps *creditFakeDeps
	logs *captureHandler
	sfx  string
	ids  []string
}

func newCreditFixture(t *testing.T) *creditFixture {
	t.Helper()
	db := testDB(t)
	deps := startCreditFakeDeps(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	f := &creditFixture{
		t: t, db: db, repo: r, deps: deps, logs: &captureHandler{},
		ctx:  besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: creditOwner, DeptPath: "/1/12/"}),
		orch: tcc.New(r, deps.clients(), "1", 2*time.Second, "u_ops_test"),
		sfx:  fmt.Sprintf("%d", time.Now().UnixNano()),
	}
	t.Cleanup(f.cleanup)
	return f
}

func (f *creditFixture) key(name string) string { return "credit-test-" + name + "-" + f.sfx }

func (f *creditFixture) draftOrder(name string) *repo.Order {
	f.t.Helper()
	o, err := f.orch.CreateOrder(f.ctx, f.key(name+"-create"), "C-credit-"+f.sfx,
		[]tcc.CreateOrderItemInput{{ProductID: "P-credit", Qty: "1"}})
	if err != nil {
		f.t.Fatalf("建单失败：%v", err)
	}
	f.ids = append(f.ids, o.ID)
	return o
}

func (f *creditFixture) confirmedOrder(name string) *repo.Order {
	f.t.Helper()
	o := f.draftOrder(name)
	confirmed, err := f.orch.ConfirmOrder(f.ctx, o.ID, f.key(name+"-confirm"), slog.Default())
	if err != nil {
		f.t.Fatalf("确认失败：%v", err)
	}
	return confirmed
}

// consumeRejection 在 inbox 事务里跑 handler，与 besdk.Consume 一样。
func (f *creditFixture) consumeRejection(orderID string) {
	f.t.Helper()
	payload := fmt.Sprintf(`{"customer_id":"C-credit-%s","order_id":%q,"exposure":"90000.00","limit":"50000.00"}`, f.sfx, orderID)
	handle := creditRejectedHandler(f.orch, slog.New(f.logs))
	err := besdk.WithTx(f.ctx, f.db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		return handle(f.ctx, tx, besdk.Event{Subject: "finance.credit.rejected.v1", AggregateID: orderID, Version: 1, Payload: []byte(payload)})
	})
	if err != nil {
		f.t.Fatalf("消费 finance.credit.rejected.v1 失败：%v", err)
	}
}

func (f *creditFixture) reload(id string) *repo.Order {
	f.t.Helper()
	o, err := f.repo.GetOrder(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

func (f *creditFixture) cleanup() {
	for _, id := range f.ids {
		_, _ = f.db.Exec(`DELETE FROM erp_sales.event_outbox WHERE aggregate_id = $1 AND subject LIKE 'sales.order.%'`, id)
		_, _ = f.db.Exec(`DELETE FROM erp_sales.sales_order_items WHERE order_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM erp_sales.sales_orders WHERE id = $1`, id)
	}
	_, _ = f.db.Exec(`DELETE FROM erp_sales.command_idempotency WHERE idempotency_key LIKE $1`, "credit-test-%-"+f.sfx)
	_, _ = f.db.Exec(`DELETE FROM erp_sales.customer_snapshots WHERE customer_id = $1`, "C-credit-"+f.sfx)
}

// noWorkflowOrchestrator 是没有装配 infra/workflow 的编排器：只走本地状态的测试用。
func noWorkflowOrchestrator(r *repo.Repo) *tcc.Orchestrator {
	return tcc.New(r, client.New(besdk.NewConfig(map[string]string{})), "1", time.Second, "")
}

// captureHandler 记下每条日志的级别。
type captureHandler struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.levels = append(h.levels, r.Level)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) count(lv slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.levels {
		if l == lv {
			n++
		}
	}
	return n
}

// creditFakeDeps 在测试进程里扮演 mdm/customer、mdm/product、erp/inventory 与
// infra/workflow，记下释放了哪些预留、开了哪些待办。
type creditFakeDeps struct {
	addr      string
	mu        sync.Mutex
	cancelled []string
	tasks     []*workflowv1.CreateTaskRequest
}

func startCreditFakeDeps(t *testing.T) *creditFakeDeps {
	t.Helper()
	d := &creditFakeDeps{}
	srv := grpc.NewServer()
	customerv1.RegisterCustomerServiceServer(srv, creditFakeCustomers{})
	productv1.RegisterProductServiceServer(srv, creditFakeProducts{})
	inventoryv1.RegisterInventoryServiceServer(srv, &creditFakeInventory{d: d})
	workflowv1.RegisterWorkflowServiceServer(srv, &creditFakeWorkflow{d: d})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	d.addr = lis.Addr().String()
	return d
}

func (d *creditFakeDeps) clients() *client.Clients {
	ep := "http://" + d.addr
	return client.New(besdk.NewConfig(map[string]string{
		"MDM_CUSTOMER_GRPC_ENDPOINT": ep, "MDM_PRODUCT_GRPC_ENDPOINT": ep,
		"ERP_INVENTORY_GRPC_ENDPOINT": ep, "INFRA_WORKFLOW_GRPC_ENDPOINT": ep,
	}))
}

func (d *creditFakeDeps) cancelledReservations() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.cancelled...)
}

func (d *creditFakeDeps) taskRequests() []*workflowv1.CreateTaskRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*workflowv1.CreateTaskRequest(nil), d.tasks...)
}

type creditFakeCustomers struct {
	customerv1.UnimplementedCustomerServiceServer
}

func (creditFakeCustomers) BatchGet(_ context.Context, req *customerv1.BatchGetRequest) (*customerv1.BatchGetResponse, error) {
	out := &customerv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Customers = append(out.Customers, &customerv1.Customer{
			Id: id, Name: "假客户", CreditLimit: "100000.00", Version: 1,
			Status: customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type creditFakeProducts struct {
	productv1.UnimplementedProductServiceServer
}

func (creditFakeProducts) BatchGet(_ context.Context, req *productv1.BatchGetRequest) (*productv1.BatchGetResponse, error) {
	out := &productv1.BatchGetResponse{}
	for _, id := range req.Ids {
		out.Products = append(out.Products, &productv1.Product{
			Id: id, Sku: "SKU-" + id, Name: "假产品", BaseUomId: "EA", Version: 1,
			Status: productv1.ProductStatus_PRODUCT_STATUS_ACTIVE,
		})
	}
	return out, nil
}

type creditFakeInventory struct {
	inventoryv1.UnimplementedInventoryServiceServer
	d *creditFakeDeps
}

func (f *creditFakeInventory) Reserve(_ context.Context, req *inventoryv1.ReserveRequest) (*inventoryv1.ReserveResponse, error) {
	return &inventoryv1.ReserveResponse{ReservationId: "R-" + req.IdempotencyKey}, nil
}

func (f *creditFakeInventory) ConfirmIssue(context.Context, *inventoryv1.ConfirmIssueRequest) (*inventoryv1.ConfirmIssueResponse, error) {
	return &inventoryv1.ConfirmIssueResponse{}, nil
}

func (f *creditFakeInventory) CancelReservation(_ context.Context, req *inventoryv1.CancelReservationRequest) (*inventoryv1.CancelReservationResponse, error) {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.d.cancelled = append(f.d.cancelled, req.ReservationId)
	return &inventoryv1.CancelReservationResponse{}, nil
}

type creditFakeWorkflow struct {
	workflowv1.UnimplementedWorkflowServiceServer
	d *creditFakeDeps
}

func (f *creditFakeWorkflow) CreateTask(_ context.Context, req *workflowv1.CreateTaskRequest) (*workflowv1.Task, error) {
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	f.d.tasks = append(f.d.tasks, req)
	return &workflowv1.Task{Id: "T-" + req.IdempotencyKey}, nil
}
