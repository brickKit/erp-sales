package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/backend/internal/repo"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
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

func natsURLForTest(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("TEST_NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}

// publishEvent 的 aggregateID 必须是每次调用都不同的值——besdk.Consume
// 的 event_inbox 按 (subject, aggregate_id, version) 做单调去重，且这张
// 表是持久化的（不会在两次 go test 之间清空），同 erp-inventory/
// erp-finance 已经踩过的坑（consumer_test.go 的既有约定）。
func publishEvent(t *testing.T, nc *nats.Conn, subject, aggregateID string, version int64, payload string) {
	t.Helper()
	msg := &nats.Msg{Subject: subject, Data: []byte(payload), Header: nats.Header{}}
	msg.Header.Set("X-Aggregate-Id", aggregateID)
	msg.Header.Set("X-Version", strconv.FormatInt(version, 10))
	msg.Header.Set("X-Hop-Count", "0")
	if err := nc.PublishMsg(msg); err != nil {
		t.Fatal(err)
	}
}

// TestConsumer_权威额度超限转SUSPENDED 验证消费 finance.credit.rejected.v1
// 这条链路——真订阅、真发布、真等待，不是直接调 repo.SuspendOrderTx。
func TestConsumer_权威额度超限转SUSPENDED(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	r := repo.New(db, "erp_sales_rw", "erp_sales")
	order, err := r.CreateOrder(context.Background(), repo.CreateOrderInput{
		IdempotencyKey: fmt.Sprintf("consumer-suspend-create-%d", time.Now().UnixNano()),
		CustomerID:     fmt.Sprintf("consumer-suspend-cust-%d", time.Now().UnixNano()), CustomerName: "消费测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-consumer", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "1", Discount: "0", TaxRate: "0", Subtotal: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", "finance.credit.rejected.v1",
			creditRejectedHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	payload := fmt.Sprintf(`{"customer_id":"C-x","order_id":%q,"exposure":"90000.00","limit":"50000.00"}`, order.ID)
	publishEvent(t, nc, "finance.credit.rejected.v1", order.ID, 1, payload)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	reloaded, err := r.GetOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != repo.StatusSuspended {
		t.Fatalf("期望 SUSPENDED，实际 %q", reloaded.Status)
	}
	if reloaded.SuspendedReason == "" {
		t.Fatal("期望 suspended_reason 有内容")
	}
}

// TestConsumer_客户事件维护信用额度快照 验证消费
// mdm.customer.created.v1/.updated.v1 这条链路。
func TestConsumer_客户事件维护信用额度快照(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	customerID := fmt.Sprintf("consumer-snapshot-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", "mdm.customer.created.v1",
			customerSnapshotHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	payload := fmt.Sprintf(`{"id":%q,"credit_limit":"12345.00"}`, customerID)
	publishEvent(t, nc, "mdm.customer.created.v1", customerID, 1, payload)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	r := repo.New(db, "erp_sales_rw", "erp_sales")
	limit, exposure, err := r.GetCustomerSnapshot(context.Background(), customerID)
	if err != nil {
		t.Fatal(err)
	}
	if limit != "12345.00" {
		t.Fatalf("期望 credit_limit=12345.00，实际 %q", limit)
	}
	// ⚠️ 这里的 "0.00" 不是 GetCustomerSnapshotTx 那个"查不到就返回 0"的
	// 兜底分支——UpsertCustomerSnapshotCreditLimitTx 已经真的插入了一行
	// （只是 credit_exposure 用了列默认值），NUMERIC(18,2) 的默认值格式
	// 是 "0.00" 不是 "0"，两者是不同的代码路径，断言不能混着比。
	if exposure != "0.00" {
		t.Fatalf("期望 credit_exposure 仍是列默认值 0.00（这条事件不该动它），实际 %q", exposure)
	}
}
