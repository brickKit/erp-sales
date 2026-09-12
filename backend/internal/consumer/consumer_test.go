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

// testSubject 给消费者测试造一个测试私有的 subject，不直接用生产真实
// subject。
//
// ⚠️ 实测踩坑（docs/dev/field-tested-pitfalls-log.md 类别 E 的 E2）：这几条
// 测试原来直接订阅/发布到真实 subject（如 "finance.credit.rejected.v1"），
// 而同一台机器上 `brickkit up` 真实跑着的 erp-sales 容器订阅的是**同一个**
// subject——NATS 核心发布订阅对同一 subject 的多个订阅者是广播，两边都会
// 收到测试发布的消息，谁先把 event_inbox 那一行 INSERT 成功谁就真正执行
// handler，断言读到的可能是真实容器的产出，不是本地被测代码的产出。
//
// 换一个测试私有的 subject 就能让真实容器完全收不到——它们只订阅生产
// subject 字面量，不会去猜一个带随机后缀的名字。这个换法是安全的：
// besdk.Consume 的 fn 只用 ev.Subject 拼错误信息，不拿它做任何业务判断，
// 换成任意字符串不影响被测逻辑本身。这条规避法只适用于"测试直接构造/
// 发布事件"的消费者测试——验证"真的发到了生产 subject 上"这件事本身的
// 测试必须用真实 subject，不适用这个换法（本文件没有这类测试）。
func testSubject(base string) string {
	return fmt.Sprintf("test.%s.%d", base, time.Now().UnixNano())
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

	subj := testSubject("finance.credit.rejected.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", subj,
			creditRejectedHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	payload := fmt.Sprintf(`{"customer_id":"C-x","order_id":%q,"exposure":"90000.00","limit":"50000.00"}`, order.ID)
	publishEvent(t, nc, subj, order.ID, 1, payload)
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
	subj := testSubject("mdm.customer.created.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", subj,
			customerSnapshotHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	payload := fmt.Sprintf(`{"id":%q,"credit_limit":"12345.00"}`, customerID)
	publishEvent(t, nc, subj, customerID, 1, payload)
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

// createSuspendedOrderForConsumerTest 造一张真的因补偿连续失败 3 次而
// SUSPENDED 的订单——同 repo.suspend_test.go 的前置状态，走真实的
// IncrementCompensationAttempts 而不是直接拼 SQL，确保 suspended_reason
// 是 ExceptionReasonCompensationFailed 这个真实常量。
func createSuspendedOrderForConsumerTest(t *testing.T, r *repo.Repo) *repo.Order {
	t.Helper()
	order, err := r.CreateOrder(context.Background(), repo.CreateOrderInput{
		IdempotencyKey: fmt.Sprintf("consumer-workflow-create-%d", time.Now().UnixNano()),
		CustomerID:     fmt.Sprintf("consumer-workflow-cust-%d", time.Now().UnixNano()), CustomerName: "workflow消费测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-consumer-workflow", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "1", Discount: "0", TaxRate: "0", Subtotal: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.IncrementCompensationAttempts(context.Background(), order.ID); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := r.GetOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != repo.StatusSuspended {
		t.Fatalf("前置状态不对：期望 SUSPENDED，实际 %q", reloaded.Status)
	}
	return reloaded
}

// TestConsumer_补偿异常待办完成后恢复订单 验证设计计划 §4.4.4 的落地
// 闭环：真发布 infra.workflow.task.completed.v1（action: APPROVED，
// source 指向这张真实挂起的订单）→ 真订阅 → 真的把订单从 SUSPENDED
// 恢复回 DRAFT。这条测试直接对应阶段三 Task 8 的验证标准："处理这条
// 待办后 erp-sales 收到对应事件、订单状态跟着变"。
func TestConsumer_补偿异常待办完成后恢复订单(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	r := repo.New(db, "erp_sales_rw", "erp_sales")
	order := createSuspendedOrderForConsumerTest(t, r)

	subj := testSubject("infra.workflow.task.completed.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", subj,
			workflowTaskCompletedHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	// ⚠️ aggregate_id 必须每次运行都不同（同本文件 publishEvent 的既有
	// 注释：event_inbox 按 (subject, aggregate_id, version) 单调去重且
	// 持久化，不会在两次 go test 之间清空）——用 order.ID 天然满足，
	// 每次都是一笔全新订单；这里曾经写死过字面量 "999"，第二次跑
	// 这条测试时被去重表悄悄吞掉，看起来像是"恢复没生效"，实际是测试
	// 自己的幂等键复用踩了这张表的既有判据。
	taskID := "task-" + order.ID
	payload := fmt.Sprintf(
		`{"task_id":%q,"action":"APPROVED","actor_sub":"u_reviewer","comment":"已人工处理",`+
			`"source_component":"erp/sales","source_aggregate":"sales_order","source_id":%q}`, taskID, order.ID)
	publishEvent(t, nc, subj, order.ID, 1, payload)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	reloaded, err := r.GetOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != repo.StatusDraft {
		t.Fatalf("期望恢复回 DRAFT，实际 %q", reloaded.Status)
	}
	if reloaded.CompensationAttempts != 0 {
		t.Fatalf("期望 compensation_attempts 清零，实际 %d", reloaded.CompensationAttempts)
	}
}

// TestConsumer_workflow事件过滤不属于自己的记录 验证 source_component/
// source_aggregate/action 三层过滤——infra.workflow.task.completed.v1
// 是全平台共用的 subject，不能假设收到的每一条都是自己的 sales_order。
func TestConsumer_workflow事件过滤不属于自己的记录(t *testing.T) {
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	r := repo.New(db, "erp_sales_rw", "erp_sales")
	order := createSuspendedOrderForConsumerTest(t, r)

	subj := testSubject("infra.workflow.task.completed.v1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = besdk.Consume(ctx, nc, db, "erp_sales_rw", "erp_sales", subj,
			workflowTaskCompletedHandler())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)

	// 三条都不该触发恢复：来源组件不对、来源聚合不对、action 是 REJECTED。
	notMine := fmt.Sprintf(
		`{"task_id":"1","action":"APPROVED","source_component":"erp/purchase","source_aggregate":"sales_order","source_id":%q}`, order.ID)
	wrongAggregate := fmt.Sprintf(
		`{"task_id":"2","action":"APPROVED","source_component":"erp/sales","source_aggregate":"purchase_order","source_id":%q}`, order.ID)
	rejected := fmt.Sprintf(
		`{"task_id":"3","action":"REJECTED","source_component":"erp/sales","source_aggregate":"sales_order","source_id":%q}`, order.ID)
	// aggregate_id 派生自 order.ID（每次运行都不同）+ 固定后缀区分三条，
	// 同 TestConsumer_补偿异常待办完成后恢复订单 的既有教训：event_inbox
	// 的去重是持久化的，写死字面量在第二次运行时会被去重表悄悄吞掉。
	publishEvent(t, nc, subj, order.ID+"-not-mine-1", 1, notMine)
	publishEvent(t, nc, subj, order.ID+"-not-mine-2", 1, wrongAggregate)
	publishEvent(t, nc, subj, order.ID+"-not-mine-3", 1, rejected)
	nc.Flush()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	reloaded, err := r.GetOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != repo.StatusSuspended {
		t.Fatalf("三条都不该恢复这张订单，期望仍是 SUSPENDED，实际 %q", reloaded.Status)
	}
}
