// 真故障注入测试（阶段三 Task 14）：HandleOpportunityWon 是
// crm.opportunity.won.v1 的消费入口，真的调 mdm-customer/mdm-product/
// erp-inventory（走系统身份，见 opportunity_won.go 顶部注释），断言真实
// 建单+确认成功，以及库存不足时订单确实没有落库成 CONFIRMED、库存确实
// 没被占。复用 confirm_test.go 同一个包里已有的 requireE2EEnv/testDB/
// uniqueSuffix/createRealCustomer/createRealProduct/receiveRealStock/
// getRealBalance/newTestOrchestrator 辅助函数。
package tcc

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	workflowv1 "github.com/brickKit/erp-sales/gen/infra/workflow/v1"
)

func TestHandleOpportunityWon_真实建单确认成功(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	rdb := realDB(t)
	ctx := e2eTestCtx() // 只用于建测试客户/产品这两个 setup 步骤，同 confirm_test.go 的既有判据
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00")
	seedCustomerSnapshot(t, db, customerID, "100000.00")
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, rdb, productID, "50")

	before := getRealBalance(t, ctx, rdb, productID)

	opportunityID := uniqueSuffix("test-opp-won")
	payload := OpportunityWonPayload{
		OpportunityID: opportunityID, CustomerID: customerID,
		Items:   []OpportunityWonItem{{ProductID: productID, Quantity: "3", QuotedUnitPrice: "999.00"}},
		OwnerID: "u_won_test_owner", DeptPath: "/1/12/", Currency: "CNY", Revision: 1,
	}

	// HandleOpportunityWon 是"尽力而为"，不返回 error（见其函数注释）——
	// 通过真实 DB 状态断言结果，不是看返回值。
	orch.HandleOpportunityWon(context.Background(), payload, slog.Default())

	var status, ownerID, deptPath, deptID string
	if err := db.QueryRowContext(context.Background(), `
		SELECT status, owner_id, dept_path, dept_id FROM erp_sales.sales_orders
		WHERE customer_id = $1 ORDER BY id DESC LIMIT 1`, customerID,
	).Scan(&status, &ownerID, &deptPath, &deptID); err != nil {
		t.Fatalf("查订单失败（期望真的建出一条订单）: %v", err)
	}
	if status != repo.StatusConfirmed {
		t.Fatalf("期望 CONFIRMED，实际 %q", status)
	}
	// ⭐ owner_id/dept_path 必须来自事件本身，不是 ctx 派生——事件 handler
	// 语境下根本没有 JWT/ScopeOf 可用（crm-opportunity 设计计划 §4.1）。
	if ownerID != "u_won_test_owner" || deptPath != "/1/12/" || deptID != "12" {
		t.Fatalf("订单归属字段应该来自事件 payload，实际 owner_id=%q dept_path=%q dept_id=%q", ownerID, deptPath, deptID)
	}

	// 断言库存真的被占住了——不是订单侧自己说了算。
	after := getRealBalance(t, ctx, rdb, productID)
	beforeReserved, _ := strconv.ParseFloat(before.ReservedQty, 64)
	afterReserved, _ := strconv.ParseFloat(after.ReservedQty, 64)
	if afterReserved-beforeReserved != 3 {
		t.Fatalf("期望 reserved_qty 增加 3，实际增加 %v", afterReserved-beforeReserved)
	}
}

// TestHandleOpportunityWon_库存不足时不建单确认 是阶段三 Task 14 明文
// 要求的故障注入之一："真实制造一次库存不足，确认 erp-sales 的补偿链
// 依然正确（订单没落库、库存没被占）"。crm-opportunity 侧的赢单状态
// 不回滚由 Task 14 的 brickkit up 全链路验证覆盖（本组件从不回传失败
// 给 crm-opportunity，见 opportunity_won.go 顶部注释）。
func TestHandleOpportunityWon_库存不足时不建单确认(t *testing.T) {
	requireE2EEnv(t)
	requireWorkflowEnv(t)
	db := testDB(t)
	rdb := realDB(t)
	ctx := e2eTestCtx()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00")
	seedCustomerSnapshot(t, db, customerID, "100000.00")
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, rdb, productID, "2") // 只入 2 件

	before := getRealBalance(t, ctx, rdb, productID)

	opportunityID := uniqueSuffix("test-opp-won-insufficient")
	ownerSub := uniqueSuffix("owner")
	payload := OpportunityWonPayload{
		OpportunityID: opportunityID, CustomerID: customerID,
		Items:   []OpportunityWonItem{{ProductID: productID, Quantity: "999", QuotedUnitPrice: "1.00"}}, // 明显不够
		OwnerID: ownerSub, DeptPath: "/1/13/", Currency: "CNY", Revision: 1,
	}

	orch.HandleOpportunityWon(context.Background(), payload, slog.Default())

	var status string
	if err := db.QueryRowContext(context.Background(), `
		SELECT status FROM erp_sales.sales_orders WHERE customer_id = $1 ORDER BY id DESC LIMIT 1`, customerID,
	).Scan(&status); err != nil {
		t.Fatalf("查订单失败: %v", err)
	}
	if status != repo.StatusDraft {
		t.Fatalf("库存明显不够，订单不该被确认成 CONFIRMED，期望仍是 DRAFT，实际 %q", status)
	}

	// 库存确实没被占住。
	after := getRealBalance(t, ctx, rdb, productID)
	if after.ReservedQty != before.ReservedQty {
		t.Fatalf("库存不足的 Reserve 不该留下任何占用，before=%s after=%s", before.ReservedQty, after.ReservedQty)
	}

	// 真的建了一条异常待办，assignee 是事件里带的 owner_id（不是静态配置的
	// exceptionAssigneeSub——那是另一条独立路径，见 opportunity_won.go 的
	// createOpportunityExceptionTask 注释）。
	wfConn, closeWf, err := client.WorkflowSystem()
	if err != nil {
		t.Fatalf("拨号 infra-workflow 失败: %v", err)
	}
	defer closeWf()
	statusResp, err := wfConn.GetTaskStatus(context.Background(), &workflowv1.GetTaskStatusRequest{
		IdempotencyKey: "crm-won:" + opportunityID + ":exception-task",
	})
	if err != nil {
		t.Fatalf("GetTaskStatus 失败: %v", err)
	}
	if statusResp.Status != workflowv1.TaskStatus_TASK_STATUS_PENDING {
		t.Fatalf("期望异常待办是 PENDING，实际 %v", statusResp.Status)
	}
	tasks, err := wfConn.BatchGetTasks(context.Background(), &workflowv1.BatchGetTasksRequest{TaskIds: []string{statusResp.TaskId}})
	if err != nil {
		t.Fatalf("BatchGetTasks 失败: %v", err)
	}
	if len(tasks.Tasks) != 1 {
		t.Fatalf("期望查到 1 条待办，实际 %d 条", len(tasks.Tasks))
	}
	task := tasks.Tasks[0]
	if task.AssigneeSub != ownerSub {
		t.Fatalf("期望 assignee_sub=事件的 owner_id=%q，实际 %q", ownerSub, task.AssigneeSub)
	}
	if task.SourceComponent != "erp/sales" || task.SourceAggregate != "opportunity_conversion" || task.SourceId != opportunityID {
		t.Fatalf("来源四元组不对：%+v", task)
	}
}
