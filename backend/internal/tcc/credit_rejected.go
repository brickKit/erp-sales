package tcc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
)

// OpenCreditRejectedTask 在财务拒绝一张订单的信用（finance.credit.rejected.v1）
// 之后开一条 infra/workflow 异常待办，派给订单负责人——他认识这个客户，也是能
// 取消这张单的人（数据范围内）。两种情形：
//   - 订单已挂起：它占着库存，取消（会先释放预留）或与客户谈妥后再处理；
//   - 订单已发货（shipped）：没有挂起，货已出库，超额度这件事只能人跟进。
//
// 只在事件 handler 里调用，所以走系统身份。幂等键由订单 id 派生：同一张单
// 只有一条这样的待办，事件再来一次拿到的是同一条。待办被"同意"不会自动做任何
// 事（ResumeFromExceptionTx 只认补偿挂起的 reason）。workflow 缺席、负责人为空、
// 调用失败都只记日志：订单状态已经落下，待办是通知，不是事务的一部分。
func (o *Orchestrator) OpenCreditRejectedTask(ctx context.Context, order *repo.Order, exposure, limit string, shipped bool, logger *slog.Logger) {
	if !o.Clients.WorkflowConfigured() {
		logger.Info("infra/workflow 未装配，跳过建信用拒绝的异常待办", "order_id", order.ID)
		return
	}
	assignee := order.OwnerID
	if assignee == "" {
		assignee = o.ExceptionAssigneeSub
	}
	if assignee == "" {
		logger.Warn("订单没有负责人、EXCEPTION_ASSIGNEE_SUB 也未配置，跳过建信用拒绝的异常待办", "order_id", order.ID)
		return
	}

	wfConn, closeWf, err := o.Clients.WorkflowSystem()
	if err != nil {
		logger.Error("拨号 infra-workflow 失败，跳过建信用拒绝的异常待办", "order_id", order.ID, "error", err)
		return
	}
	defer closeWf()

	title := fmt.Sprintf("订单 %s 被财务拒绝信用，已挂起并占着库存：取消订单或与客户沟通", order.OrderNo)
	if shipped {
		title = fmt.Sprintf("订单 %s 已发货，财务随后拒绝了它的信用：请跟进应收", order.OrderNo)
	}
	summaryJSON, err := json.Marshal(map[string]string{
		"order_id": order.ID, "customer_id": order.CustomerID, "exposure": exposure, "limit": limit,
	})
	if err != nil {
		logger.Error("序列化异常待办摘要失败", "order_id", order.ID, "error", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	_, err = wfConn.CreateTask(callCtx, &workflowv1.CreateTaskRequest{
		IdempotencyKey: "credit-rejected:" + order.ID + ":exception-task",
		Type:           workflowv1.TaskType_TASK_TYPE_EXCEPTION,
		AssigneeSub:    assignee,
		// 订单的部门快照：部门里看得到这张单的人也看得到这条待办；空串时只有
		// 负责人本人看得到（与 workflow 的约定一致）。
		AssigneeDeptPath: order.DeptPath,
		Title:            title,
		SummaryJson:      string(summaryJSON),
		SourceComponent:  "erp/sales",
		SourceAggregate:  "sales_order",
		SourceId:         order.ID,
		DeepLink:         "/erp/sales/orders/" + order.ID,
	})
	if err != nil {
		logger.Error("建信用拒绝的异常待办失败", "order_id", order.ID, "error", err)
		return
	}
	logger.Info("已建信用拒绝的异常待办", "order_id", order.ID, "assignee_sub", assignee, "shipped", shipped)
}
