package repo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // §12.4：不用 lib/pq，驱动名注册为 "pgx"
)

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
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatInt(n, 10)
}

func testItems() []CreateOrderItemInput {
	return []CreateOrderItemInput{
		{ProductID: "P-TEST-1", ProductSKU: "SKU-1", ProductName: "测试产品1", UOMID: "EA",
			Qty: "2", UnitPrice: "100.00", Discount: "0", TaxRate: "0.13", Subtotal: "200.00"},
	}
}

func TestCreateOrder_基本建单(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("create"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != StatusDraft {
		t.Fatalf("期望 DRAFT，实际 %q", order.Status)
	}
	if order.OrderNo != "SO"+order.ID {
		t.Fatalf("order_no 期望 SO+id=%q，实际 %q", "SO"+order.ID, order.OrderNo)
	}
	if order.TotalAmount != "200.00" {
		t.Fatalf("期望 total_amount=200.00，实际 %q", order.TotalAmount)
	}
	if len(order.Items) != 1 || order.Items[0].ProductID != "P-TEST-1" {
		t.Fatalf("订单行没有正确落库：%+v", order.Items)
	}
}

func TestCreateOrder_幂等(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	key := uniqueSuffix("create-idem")

	in := CreateOrderInput{IdempotencyKey: key, CustomerID: "C-2", CustomerName: "测试客户2", Items: testItems()}
	o1, err := r.CreateOrder(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := r.CreateOrder(ctx, in)
	if err != nil {
		t.Fatalf("幂等重试报错了：%v", err)
	}
	if o1.ID != o2.ID {
		t.Fatalf("幂等失效：第一次 id=%s，第二次 id=%s", o1.ID, o2.ID)
	}
}

func TestFinalizeConfirm_DRAFT到CONFIRMED且发事件(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("confirm-create"), CustomerID: "C-3", CustomerName: "客户3", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}

	confirmed, err := r.FinalizeConfirm(ctx, FinalizeConfirmInput{
		IdempotencyKey: uniqueSuffix("confirm"), OrderID: order.ID, ReservationID: "999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != StatusConfirmed {
		t.Fatalf("期望 CONFIRMED，实际 %q", confirmed.Status)
	}
	if confirmed.ReservationID != "999" {
		t.Fatalf("期望 reservation_id=999，实际 %q", confirmed.ReservationID)
	}
	if confirmed.Version != order.Version+1 {
		t.Fatalf("期望 version 递增 1，原=%d 现=%d", order.Version, confirmed.Version)
	}

	// 断言 outbox 里真的写了一条 sales.order.created.v1（同 erp-finance/
	// erp-inventory 的既有判据：outbox 是本地事务的一部分，不是靠 mock）。
	var count int
	err = db.QueryRowContext(ctx, `
		SELECT count(*) FROM erp_sales.event_outbox
		WHERE subject = 'sales.order.created.v1' AND aggregate_id = $1`, order.ID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("期望恰好 1 条 sales.order.created.v1 outbox 记录，实际 %d", count)
	}
}

func TestFinalizeConfirm_非DRAFT状态拒绝(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("confirm-twice-create"), CustomerID: "C-4", CustomerName: "客户4", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeConfirm(ctx, FinalizeConfirmInput{
		IdempotencyKey: uniqueSuffix("confirm-1"), OrderID: order.ID, ReservationID: "1",
	}); err != nil {
		t.Fatal(err)
	}

	// 第二次用不同的 idempotency_key 确认同一张已经 CONFIRMED 的订单——
	// 必须被最后一道防线拦下，不能把 reservation_id 覆盖成另一个值
	// （设计计划 §3.1：claim-first 只防同一个 key 重复，两个不同 key 打
	// 同一个 order_id 是客户端误用，靠锁行重新判断状态兜底）。
	_, err = r.FinalizeConfirm(ctx, FinalizeConfirmInput{
		IdempotencyKey: uniqueSuffix("confirm-2"), OrderID: order.ID, ReservationID: "2",
	})
	if !errors.Is(err, ErrOrderNotDraft) {
		t.Fatalf("期望 ErrOrderNotDraft，实际：%v", err)
	}
}

func TestFinalizeCancel_DRAFT直接取消不需要预留(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("cancel-draft-create"), CustomerID: "C-5", CustomerName: "客户5", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := r.FinalizeCancel(ctx, FinalizeCancelInput{
		IdempotencyKey: uniqueSuffix("cancel"), OrderID: order.ID, Reason: "测试取消",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("期望 CANCELLED，实际 %q", cancelled.Status)
	}
}

func TestFinalizeCancel_已取消再取消是幂等(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("cancel-twice-create"), CustomerID: "C-6", CustomerName: "客户6", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeCancel(ctx, FinalizeCancelInput{
		IdempotencyKey: uniqueSuffix("cancel-1"), OrderID: order.ID, Reason: "第一次",
	}); err != nil {
		t.Fatal(err)
	}
	// 不同 idempotency_key 再取消一次已经 CANCELLED 的订单——必须如实
	//返回当前状态，不报错（设计计划 §2.1：CANCELLED 是终态，但重复取消
	// 请求本身应该是幂等的，不是错误）。
	again, err := r.FinalizeCancel(ctx, FinalizeCancelInput{
		IdempotencyKey: uniqueSuffix("cancel-2"), OrderID: order.ID, Reason: "第二次",
	})
	if err != nil {
		t.Fatalf("重复取消不该报错：%v", err)
	}
	if again.Status != StatusCancelled {
		t.Fatalf("期望仍是 CANCELLED，实际 %q", again.Status)
	}
}

func TestFinalizeCancel_终态订单不能取消(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("cancel-terminal-create"), CustomerID: "C-7", CustomerName: "客户7", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeConfirm(ctx, FinalizeConfirmInput{
		IdempotencyKey: uniqueSuffix("confirm"), OrderID: order.ID, ReservationID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeShip(ctx, FinalizeShipInput{
		IdempotencyKey: uniqueSuffix("ship"), OrderID: order.ID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.FinalizeCancel(ctx, FinalizeCancelInput{
		IdempotencyKey: uniqueSuffix("cancel"), OrderID: order.ID, Reason: "太晚了",
	})
	if !errors.Is(err, ErrOrderTerminal) {
		t.Fatalf("SHIPPED 订单不该能取消，期望 ErrOrderTerminal，实际：%v", err)
	}
}

func TestFinalizeShip_CONFIRMED到SHIPPED且发事件(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("ship-create"), CustomerID: "C-8", CustomerName: "客户8", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinalizeConfirm(ctx, FinalizeConfirmInput{
		IdempotencyKey: uniqueSuffix("confirm"), OrderID: order.ID, ReservationID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	shipped, err := r.FinalizeShip(ctx, FinalizeShipInput{
		IdempotencyKey: uniqueSuffix("ship"), OrderID: order.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if shipped.Status != StatusShipped {
		t.Fatalf("期望 SHIPPED，实际 %q", shipped.Status)
	}

	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM erp_sales.event_outbox
		WHERE subject = 'sales.order.shipped.v1' AND aggregate_id = $1`, order.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("期望恰好 1 条 sales.order.shipped.v1 outbox 记录，实际 %d", count)
	}
}

func TestFinalizeShip_DRAFT不能直接发货(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("ship-draft-create"), CustomerID: "C-9", CustomerName: "客户9", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.FinalizeShip(ctx, FinalizeShipInput{IdempotencyKey: uniqueSuffix("ship"), OrderID: order.ID})
	if !errors.Is(err, ErrOrderNotDraft) {
		t.Fatalf("DRAFT 订单不该能发货，实际：%v", err)
	}
}

func TestEnqueueReconciliation_写入(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	order, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("reconcile-create"), CustomerID: "C-10", CustomerName: "客户10", Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnqueueReconciliation(ctx, order.ID, "", "测试：Reserve 超时且查询也超时"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM erp_sales.sales_order_reconciliation_queue
		WHERE order_id = $1 AND resolved_at IS NULL`, order.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("期望恰好 1 条待处理的对账记录，实际 %d", count)
	}
}

func TestGetCustomerSnapshot_查不到时返回0(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	limit, exposure, err := r.GetCustomerSnapshot(ctx, uniqueSuffix("never-seen-customer"))
	if err != nil {
		t.Fatal(err)
	}
	if limit != "0" || exposure != "0" {
		t.Fatalf("查不到的客户应该返回额度 0、已用额度 0，实际 limit=%q exposure=%q", limit, exposure)
	}
}
