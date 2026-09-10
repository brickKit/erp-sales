// 阶段三 Task 14 明文要求的验证之一："DLQ 进出各一遍：重试耗尽后消息
// 真的进 DLQ、hop_count > 5 真的被丢弃、能被重新投递——这是阶段二
// be-sdk-go 的 SDK 层能力，本阶段第一次在真实业务场景下触发它"。
//
// 这条测试用 crm.opportunity.won.v1（本阶段新增的真实业务事件）而不是
// 一个合成的占位 subject，满足"真实业务场景"这一条；DLQ 判定逻辑本身
// （hop_count > 5 直接转发到 dlq.<subject>，不调用 fn）完全在
// besdk.Consume 内部，这里只是首次让它在这条真实链路上触发一次。
//
// ⚠️ 用一个刻意不存在的 customer_id，不需要真的建一条活跃客户——
// "fn 被调用过、且真的报了客户不存在、真的建了异常待办"与"fn 根本没被
// 调用（DLQ 分支）"两种状态足以区分"进 DLQ"与"正常投递"，不需要走到
// Reserve 库存那一步才能证明"消息被处理过"。这样只需要
// MDM_CUSTOMER_GRPC_ENDPOINT/INFRA_WORKFLOW_GRPC_ENDPOINT 两个依赖真的
// 可达，不需要额外造活跃客户/产品数据。
package consumer

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/tcc"

	workflowv1 "github.com/brickKit/erp-sales/gen/infra/workflow/v1"
)

func requireOpportunityWonDLQEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MDM_CUSTOMER_GRPC_ENDPOINT", "INFRA_WORKFLOW_GRPC_ENDPOINT"} {
		if _, ok := os.LookupEnv(k); !ok {
			t.Skipf("未设置 %s，跳过（需要依赖组件真的在跑）", k)
		}
	}
}

// publishOpportunityWon 直接构造 nats.Msg（不复用 publishEvent，因为要
// 精确控制 hop_count，publishEvent 硬编码成 "0"）。
func publishOpportunityWon(t *testing.T, nc *nats.Conn, aggregateID, hopCount, payload string) {
	t.Helper()
	msg := &nats.Msg{Subject: "crm.opportunity.won.v1", Data: []byte(payload), Header: nats.Header{}}
	msg.Header.Set("X-Aggregate-Id", aggregateID)
	msg.Header.Set("X-Version", "1")
	msg.Header.Set("X-Hop-Count", hopCount)
	if err := nc.PublishMsg(msg); err != nil {
		t.Fatal(err)
	}
}

// TestOpportunityWon_hopCount超限进DLQ且可重新投递 分两段：① hop_count=6
// 的消息不会被 opportunityWonHandler 处理（连客户查询都不会真的发生），
// 而是真的出现在 dlq.crm.opportunity.won.v1；② 把同一份 payload 用
// hop_count=0 重新投递到原 subject（换一个 aggregate_id 避开
// event_inbox 持久化去重），这次真的被 fn 摸到——客户不存在会走异常
// 待办分支，用"异常待办真的被建出来"证明 fn 这次真的执行了，与①形成
// 对照，证明 DLQ 不是"丢了就真的丢了"，人工重放这个动作本身是可行的
// （infra-dlq-monitor 的管理界面不在本阶段范围，这里验的是 SDK 层
// 机制本身）。
func TestOpportunityWon_hopCount超限进DLQ且可重新投递(t *testing.T) {
	requireOpportunityWonDLQEnv(t)
	db := testDB(t)
	nc, err := nats.Connect(natsURLForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	dlqCh := make(chan *nats.Msg, 4)
	dlqSub, err := nc.Subscribe("dlq.crm.opportunity.won.v1", func(m *nats.Msg) { dlqCh <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer dlqSub.Unsubscribe()
	nc.Flush()

	r := repo.New(db, "erp_sales_rw", "erp_sales")
	orch := tcc.New(r, "1", 5*time.Second, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Start(ctx, db, "erp_sales_rw", "erp_sales", nc, orch, slog.Default()) }()
	time.Sleep(150 * time.Millisecond)

	oppID := "dlq-test-" + time.Now().Format("20060102150405.000000000")
	payload := `{"opportunity_id":"` + oppID + `","customer_id":"nonexistent-customer-for-dlq-test",` +
		`"items":[{"product_id":"nonexistent-product","quantity":"1","quoted_unit_price":"1.00"}],` +
		`"owner_id":"u_dlq_test","dept_path":"/1/1/","currency":"CNY"}`

	publishOpportunityWon(t, nc, oppID, "6", payload) // hop_count=6 > maxHopCount=5

	select {
	case msg := <-dlqCh:
		if string(msg.Data) != payload {
			t.Fatalf("DLQ 消息 payload 与原始 payload 不一致：%s", string(msg.Data))
		}
		if msg.Header.Get("X-Dlq-Reason") == "" {
			t.Fatal("期望 DLQ 消息带 X-Dlq-Reason")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：hop_count > 5 的消息没有真的转发到 dlq.crm.opportunity.won.v1")
	}

	// 确认①这条真的没被 fn 摸过：不会有对应的异常待办。
	wfConn, closeWf, err := client.Workflow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeWf()
	statusResp, err := wfConn.GetTaskStatus(context.Background(), &workflowv1.GetTaskStatusRequest{
		IdempotencyKey: "crm-won:" + oppID + ":exception-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if statusResp.TaskId != "" {
		t.Fatalf("hop_count 超限的消息不该被 fn 处理，不该建出异常待办，实际查到 task_id=%q", statusResp.TaskId)
	}

	// ② 重新投递：同一条 payload，hop_count=0，换一个 aggregate_id 避开
	// event_inbox 持久化去重（同本包其它测试已经踩过、写进注释的坑）。
	redeliverOppID := oppID + "-redelivered"
	redeliverPayload := `{"opportunity_id":"` + redeliverOppID + `","customer_id":"nonexistent-customer-for-dlq-test",` +
		`"items":[{"product_id":"nonexistent-product","quantity":"1","quoted_unit_price":"1.00"}],` +
		`"owner_id":"u_dlq_test","dept_path":"/1/1/","currency":"CNY"}`
	publishOpportunityWon(t, nc, redeliverOppID, "0", redeliverPayload)

	deadline := time.Now().Add(5 * time.Second)
	var redeliverTaskID string
	for time.Now().Before(deadline) {
		resp, err := wfConn.GetTaskStatus(context.Background(), &workflowv1.GetTaskStatusRequest{
			IdempotencyKey: "crm-won:" + redeliverOppID + ":exception-task",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.TaskId != "" {
			redeliverTaskID = resp.TaskId
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if redeliverTaskID == "" {
		t.Fatal("重新投递（hop_count=0）应该真的被 fn 处理并建出异常待办（客户不存在），但没有查到")
	}
}
