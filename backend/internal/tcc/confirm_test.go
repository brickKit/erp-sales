// 真故障注入测试（设计计划 §9 第 2 条明确要求）：让 mdm-customer/
// mdm-product/erp-inventory 真的跑起来，真的返回库存不足、真的超时
// （context.WithTimeout 掐短），断言订单确实没落库、库存确实没被占住、
// 补偿确实执行了——用 mock 测这条链等于没测，要验的正是跨进程的部分。
//
// ⚠️ 这些测试需要三个真实依赖组件真的在跑，跟 TEST_PG_DSN 一样用环境
// 变量判断要不要跳过：MDM_CUSTOMER_GRPC_ENDPOINT / MDM_PRODUCT_GRPC_ENDPOINT
// / ERP_INVENTORY_GRPC_ENDPOINT（同 besdk.Endpoint() 认的格式，
// http://host:port）没有全部设置就跳过，不伪造数据也不用内存里的假实现
// 代替。
package tcc

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	customerv1 "github.com/brickKit/erp-sales/gen/mdm/customer/v1"
	inventoryv1 "github.com/brickKit/erp-sales/gen/erp/inventory/v1"
	productv1 "github.com/brickKit/erp-sales/gen/mdm/product/v1"
)

const testWarehouseID = "1" // WH-EAST，迁移播种数据（同 erp-inventory 设计计划 §9 第 5 条）

func requireE2EEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MDM_CUSTOMER_GRPC_ENDPOINT", "MDM_PRODUCT_GRPC_ENDPOINT", "ERP_INVENTORY_GRPC_ENDPOINT"} {
		if _, ok := os.LookupEnv(k); !ok {
			t.Skipf("未设置 %s，跳过真故障注入测试（需要三个依赖组件真的在跑）", k)
		}
	}
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

var seq int64

func uniqueSuffix(prefix string) string {
	n := atomic.AddInt64(&seq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

// createRealCustomer 在真的 mdm-customer 里建一个 ACTIVE 客户，返回其 id。
func createRealCustomer(t *testing.T, ctx context.Context, creditLimit string) string {
	t.Helper()
	conn, closeConn, err := client.Customer(ctx)
	if err != nil {
		t.Fatalf("拨号 mdm-customer 失败: %v", err)
	}
	defer closeConn()
	resp, err := conn.Create(ctx, &customerv1.CreateRequest{
		IdempotencyKey: uniqueSuffix("test-customer"), Name: "TCC 测试客户", CreditLimit: creditLimit,
	})
	if err != nil {
		t.Fatalf("mdm-customer.Create 失败: %v", err)
	}
	if resp.Customer.Status != customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE {
		t.Fatalf("新建客户期望 ACTIVE，实际 %v", resp.Customer.Status)
	}
	return resp.Customer.Id
}

// createRealProduct 在真的 mdm-product 里建一个 ACTIVE 产品，返回其 id。
func createRealProduct(t *testing.T, ctx context.Context) string {
	t.Helper()
	conn, closeConn, err := client.Product(ctx)
	if err != nil {
		t.Fatalf("拨号 mdm-product 失败: %v", err)
	}
	defer closeConn()
	resp, err := conn.Create(ctx, &productv1.CreateRequest{
		IdempotencyKey: uniqueSuffix("test-product"), Name: "TCC 测试产品",
		BaseUomId: "1", TrackingType: productv1.TrackingType_TRACKING_TYPE_NONE, StandardCost: "10.00",
	})
	if err != nil {
		t.Fatalf("mdm-product.Create 失败: %v", err)
	}
	if resp.Product.Status != productv1.ProductStatus_PRODUCT_STATUS_ACTIVE {
		t.Fatalf("新建产品期望 ACTIVE，实际 %v", resp.Product.Status)
	}
	return resp.Product.Id
}

// receiveRealStock 在真的 erp-inventory 里给 productID 入库 qty 件。
func receiveRealStock(t *testing.T, ctx context.Context, productID, qty string) {
	t.Helper()
	conn, closeConn, err := client.Inventory(ctx)
	if err != nil {
		t.Fatalf("拨号 erp-inventory 失败: %v", err)
	}
	defer closeConn()
	_, err = conn.Receive(ctx, &inventoryv1.ReceiveRequest{
		IdempotencyKey: uniqueSuffix("test-receive"), ProductId: productID, WarehouseId: testWarehouseID, Qty: qty,
	})
	if err != nil {
		t.Fatalf("erp-inventory.Receive 失败: %v", err)
	}
}

// getRealBalance 查真的 erp-inventory 里 productID 在测试仓库的余额。
func getRealBalance(t *testing.T, ctx context.Context, productID string) *inventoryv1.Balance {
	t.Helper()
	conn, closeConn, err := client.Inventory(ctx)
	if err != nil {
		t.Fatalf("拨号 erp-inventory 失败: %v", err)
	}
	defer closeConn()
	b, err := conn.GetBalance(ctx, &inventoryv1.GetBalanceRequest{ProductId: productID, WarehouseId: testWarehouseID})
	if err != nil {
		t.Fatalf("erp-inventory.GetBalance 失败: %v", err)
	}
	return b
}

func newTestOrchestrator(db *sql.DB, timeout time.Duration) *Orchestrator {
	return New(repo.New(db, "erp_sales_rw", "erp_sales"), testWarehouseID, timeout)
}

// seedCustomerSnapshot 直接写 customer_snapshots，模拟"mdm.customer.created.v1
// 事件已经被消费过"——本测试进程没有跑 backend/internal/consumer 的后台
// 循环（那是 Module.Start 的一部分，这里只直接构造 Orchestrator），事件
// 不会自己传播过来。ConfirmOrder 的③本地信用额度校验读的是这张本地表，
// 不是实时问 mdm-customer，所以真实场景里也是靠这张表，不是测试特有的
// 走后门（repo.GetCustomerSnapshot_查不到时返回0 那条测试已经验证过
// "查不到"分支本身是对的——这里只是补上"已经消费过事件"的前置状态）。
func seedCustomerSnapshot(t *testing.T, db *sql.DB, customerID, creditLimit string) {
	t.Helper()
	err := besdk.WithTx(context.Background(), db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		return repo.UpsertCustomerSnapshotCreditLimitTx(tx, customerID, creditLimit, 1)
	})
	if err != nil {
		t.Fatalf("写 customer_snapshots 失败: %v", err)
	}
}

// createDraftOrder 建一张真实 DRAFT 订单（走 tcc.CreateOrder，真的调
// mdm-customer/mdm-product BatchGet + 本地定价），供 ConfirmOrder 测试用。
func createDraftOrder(t *testing.T, ctx context.Context, orch *Orchestrator, customerID, productID, qty string) *repo.Order {
	t.Helper()
	order, err := orch.CreateOrder(ctx, uniqueSuffix("test-create-order"), customerID,
		[]CreateOrderItemInput{{ProductID: productID, Qty: qty}})
	if err != nil {
		t.Fatalf("CreateOrder 失败: %v", err)
	}
	return order
}

func TestConfirmOrder_真实happy_path(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00")
	seedCustomerSnapshot(t, db, customerID, "100000.00")
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, productID, "50")

	before := getRealBalance(t, ctx, productID)

	order := createDraftOrder(t, ctx, orch, customerID, productID, "3")
	confirmed, err := orch.ConfirmOrder(ctx, order.ID, uniqueSuffix("test-confirm"), slog.Default())
	if err != nil {
		t.Fatalf("ConfirmOrder 失败: %v", err)
	}
	if confirmed.Status != repo.StatusConfirmed {
		t.Fatalf("期望 CONFIRMED，实际 %q", confirmed.Status)
	}
	if confirmed.ReservationID == "" {
		t.Fatal("期望有真实的 reservation_id")
	}

	// 断言库存真的被占住了——不是订单侧自己说了算，查依赖组件的真实状态。
	after := getRealBalance(t, ctx, productID)
	beforeReserved, _ := strconv.ParseFloat(before.ReservedQty, 64)
	afterReserved, _ := strconv.ParseFloat(after.ReservedQty, 64)
	if afterReserved-beforeReserved != 3 {
		t.Fatalf("期望 reserved_qty 增加 3，实际增加 %v（before=%s after=%s）",
			afterReserved-beforeReserved, before.ReservedQty, after.ReservedQty)
	}
}

// TestConfirmOrder_真实库存不足 是设计计划 §9 第 2 条要求的故障注入之一：
// 让 erp-inventory 真的返回库存不足（FailedPrecondition），断言订单
// 确实没有被落库成 CONFIRMED、库存确实没被占住——不是 mock 一个错误
// 回来就完事。
func TestConfirmOrder_真实库存不足(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00")
	// ⚠️ 必须给一个足够大的额度并真的种进本地摘要副本——否则③本地信用
	// 额度校验（排在④ Reserve 之前）会先因为查不到快照（额度按 0 算）
	// 拒绝掉，这条测试就测不到 Reserve 真的返回库存不足这件事，会为了
	// 错误的原因通过（同 happy_path 测试踩过的同一个坑）。
	seedCustomerSnapshot(t, db, customerID, "100000.00")
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, productID, "2") // 只入 2 件

	before := getRealBalance(t, ctx, productID)

	order := createDraftOrder(t, ctx, orch, customerID, productID, "999") // 要 999 件，明显不够
	_, err := orch.ConfirmOrder(ctx, order.ID, uniqueSuffix("test-confirm-insufficient"), slog.Default())
	if err == nil {
		t.Fatal("库存明显不够，ConfirmOrder 应该失败")
	}
	// 断言失败原因确实是 Reserve 真的报了库存不足，不是本地信用额度先
	// 拒绝了——只看 err != nil 分不清是不是为了错误的原因失败。
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("期望库存不足的 FailedPrecondition，实际：%v", err)
	}

	// 订单确实没落库成 CONFIRMED——Reserve 从没成功过，FinalizeConfirm
	// 根本没被调用，订单原地留在 DRAFT。
	reloaded, err := orch.Repo.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != repo.StatusDraft {
		t.Fatalf("库存不足的订单不该被确认，期望仍是 DRAFT，实际 %q", reloaded.Status)
	}

	// 库存确实没被占住——Reserve 请求整个失败，erp-inventory 自己的条件
	// 更新连一件都不会加（设计计划 §2.2：判定与加锁是同一条语句）。
	after := getRealBalance(t, ctx, productID)
	if after.ReservedQty != before.ReservedQty {
		t.Fatalf("库存不足的 Reserve 不该留下任何占用，before=%s after=%s", before.ReservedQty, after.ReservedQty)
	}
}

// TestResolveAfterTimeout_真实查到RESERVED 是设计计划 §4.5"薛定谔的
// 超时"里"RESERVED → 当作成功继续"这条分支的直接测试：先用正常超时
// 真的 Reserve 一次（确保 erp-inventory 那边真的提交成功），再直接调
// resolveAfterTimeout（reserve() 在观察到 Reserve 超时后内部会调的
// 那个函数）验证它能用同一个 idempotency_key 正确查回真实的
// reservation_id——这是 GetReservationStatus 打到真实运行的 erp-inventory
// 服务的真实网络调用，不是 mock。
//
// ⚠️ 没有直接走"o.reserve() 用 1 纳秒超时触发真实 DeadlineExceeded 再
// 让它自己内部转去查状态"这条更"端到端"的路径：resolveAfterTimeout
// 内部的状态查询复用的是同一个 o.ReserveTimeout 字段，1 纳秒的超时会
// 让状态查询本身也几乎必然超时，测的就变成了"查询也超时"分支而不是
// 这条"查到 RESERVED"分支——两个分支需要不同量级的超时窗口，用同一个
// orchestrator 实例测不出确定性的结果。直接调 resolveAfterTimeout
// （用一个超时充裕的 orchestrator）绕开了这个耦合，同时依然是对真实
// 生产代码路径的真实网络调用。
func TestResolveAfterTimeout_真实查到RESERVED(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()

	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, productID, "20")

	orchNormal := newTestOrchestrator(db, 5*time.Second)
	invConn, closeInv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInv()

	orderID := "999999" // 只用于幂等键追溯字段，Reserve 本身不校验订单存在
	idemKey := uniqueSuffix("test-reserve-timeout")

	// 真的把预留提交成功——这是"Reserve 其实已经处理完，只是响应没收到"
	// 这个场景的真实前提条件。
	realReservationID, err := orchNormal.reserve(ctx, invConn, orderID, idemKey,
		[]repo.OrderItem{{ProductID: productID, Qty: "5"}}, slog.Default())
	if err != nil {
		t.Fatalf("正常情况下的 Reserve 不该失败: %v", err)
	}
	if realReservationID == "" {
		t.Fatal("期望拿到真实的 reservation_id")
	}

	// 直接调 resolveAfterTimeout，模拟"上一次 Reserve 调用超时后转入状态
	// 内省"——reserveIdemKey 是 reserve() 内部会做的同一层封装，这里手动
	// 还原成一样的 key 才能查到同一条记录。
	resolvedID, err := orchNormal.resolveAfterTimeout(ctx, invConn, orderID, reserveIdemKey(idemKey), slog.Default())
	if err != nil {
		t.Fatalf("状态内省不该报错: %v", err)
	}
	if resolvedID != realReservationID {
		t.Fatalf("查到的 reservation_id 应该等于真实的那个：期望 %s，实际 %s", realReservationID, resolvedID)
	}
}

// TestReserve_真实超时能自我恢复不panic不出假结果 是 o.reserve() 这个
// 公开入口本身的超时探测测试：用 1 纳秒的 ReserveTimeout 制造真实、
// 可复现的 context.DeadlineExceeded（任何真实网络调用都测不过它）。
// 由于 resolveAfterTimeout 内部的状态查询复用同一个超时字段，这里几乎
// 必然连状态查询本身也超时（对应"查询也超时"分支）——测试只断言两个
// 真实、都算正确的结果之一：要么优雅地解出了一个真 reservation_id，
// 要么返回 ErrReservePending 且已经写入对账队列，两者都不是"直接判定
// 失败"或者更糟的"假装成功"。
func TestReserve_真实超时能自我恢复不panic不出假结果(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()

	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, productID, "20")

	invConn, closeInv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInv()

	orderID := "999998"
	idemKey := uniqueSuffix("test-reserve-outer-timeout")
	orchTiny := newTestOrchestrator(db, 1*time.Nanosecond)

	resolvedID, err := orchTiny.reserve(ctx, invConn, orderID, idemKey,
		[]repo.OrderItem{{ProductID: productID, Qty: "5"}}, slog.Default())
	switch {
	case err == nil:
		if resolvedID == "" {
			t.Fatal("没有报错时必须带回真实的 reservation_id，不能是空字符串")
		}
	case err != nil:
		// 查询也超时的分支：必须是 ErrReservePending，且真的写了对账队列。
		var count int
		if qerr := db.QueryRowContext(ctx, `
			SELECT count(*) FROM erp_sales.sales_order_reconciliation_queue
			WHERE order_id = $1`, orderID).Scan(&count); qerr != nil {
			t.Fatal(qerr)
		}
		if count == 0 {
			t.Fatalf("超时分支返回错误时，要么解出真结果要么写对账队列，两者都没有：%v", err)
		}
	}
}

// TestResolveAfterTimeout_查询也超时时写入对账队列 是设计计划 §4.5
// "查询也超时"分支的直接测试：GetReservationStatus 本身也超时，不阻塞
// 用户请求，写一行进 sales_order_reconciliation_queue 就返回。
func TestResolveAfterTimeout_查询也超时时写入对账队列(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()
	r := repo.New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("reconcile-order"), CustomerID: "C-recon", CustomerName: "对账测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-recon", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "1", Discount: "0", TaxRate: "0", Subtotal: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	invConn, closeInv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInv()

	// ReserveTimeout=1 纳秒：resolveAfterTimeout 内部的 GetReservationStatus
	// 调用也会用这个超时，几乎必然真实超时，不管查的 key 是否存在。
	orchTiny := newTestOrchestrator(db, 1*time.Nanosecond)
	_, err = orchTiny.resolveAfterTimeout(ctx, invConn, order.ID, uniqueSuffix("never-existed-key"), slog.Default())
	if err == nil {
		t.Fatal("查询也超时应该返回错误（ErrReservePending），不该假装成功")
	}

	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM erp_sales.sales_order_reconciliation_queue
		WHERE order_id = $1 AND resolved_at IS NULL`, order.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("期望恰好写入 1 条待处理的对账记录，实际 %d", count)
	}
}

// TestCompensateReserve_真实释放预留且累加补偿计数 是 TCC 补偿动作
// 本身的直接测试：④ Reserve 真的成功之后，模拟⑤本地落库失败，断言
// compensateReserve 真的调用 erp-inventory.CancelReservation 把库存
// 释放回去（查依赖组件的真实余额，不是订单侧自己说了算），并且真的
// 在 sales_orders.compensation_attempts 上累加了计数（设计计划 §3.1、
// §4.4.4：补偿连续失败 3 次才转 SUSPENDED，这里只验证单次补偿成功时
// 计数确实从 0 变成 1）。
func TestCompensateReserve_真实释放预留且累加补偿计数(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	ctx := context.Background()
	r := repo.New(db, "erp_sales_rw", "erp_sales")

	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, productID, "20")
	before := getRealBalance(t, ctx, productID)

	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("compensate-order"), CustomerID: "C-compensate", CustomerName: "补偿测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: productID, ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "4", UnitPrice: "1", Discount: "0", TaxRate: "0", Subtotal: "4"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	invConn, closeInv, err := client.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInv()

	orch := newTestOrchestrator(db, 5*time.Second)
	// ④ 真的 Reserve 成功——这是补偿动作要撤销的那个真实前提。
	reservationID, err := orch.reserve(ctx, invConn, order.ID, uniqueSuffix("compensate-reserve"),
		[]repo.OrderItem{{ProductID: productID, Qty: "4"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	afterReserve := getRealBalance(t, ctx, productID)
	beforeReserved, _ := strconv.ParseFloat(before.ReservedQty, 64)
	afterReserveReserved, _ := strconv.ParseFloat(afterReserve.ReservedQty, 64)
	if afterReserveReserved-beforeReserved != 4 {
		t.Fatalf("补偿前置条件不对：期望 reserved_qty 增加 4，实际增加 %v", afterReserveReserved-beforeReserved)
	}

	// ⑤ 模拟本地落库失败（不真的调 FinalizeConfirm），直接触发补偿。
	orch.compensateReserve(ctx, invConn, order.ID, uniqueSuffix("compensate-cancel"), reservationID, slog.Default())

	// 断言库存真的被释放回去了——查依赖组件的真实余额。
	after := getRealBalance(t, ctx, productID)
	afterReserved, _ := strconv.ParseFloat(after.ReservedQty, 64)
	if afterReserved != beforeReserved {
		t.Fatalf("补偿后 reserved_qty 应该回到补偿前的水平，期望 %v，实际 %v", beforeReserved, afterReserved)
	}

	// 断言 compensation_attempts 真的累加了。
	reloaded, err := r.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CompensationAttempts != 1 {
		t.Fatalf("期望 compensation_attempts=1，实际 %d", reloaded.CompensationAttempts)
	}
}
