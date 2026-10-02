// Package consumer 消费五个 subject：
//   - finance.credit.rejected.v1：权威额度判定超限 → 订单转 SUSPENDED；
//   - mdm.customer.created.v1 / .updated.v1：维护 customer_snapshots.credit_limit；
//   - infra.workflow.task.completed.v1：补偿异常待办被人确认已处理 → 订单从
//     SUSPENDED 恢复回 DRAFT；
//   - crm.opportunity.won.v1：赢单自动转订单（见 backend/internal/tcc/opportunity_won.go）。
//
// 不消费 finance.voucher.posted.v1：它没有 customer_id，对不上客户；
// credit_exposure 要靠与 erp/finance 对账维护（还没有实现，见 docs/design.md
// 的未决问题）。
package consumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/nats-io/nats.go"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/tcc"
)

func Start(ctx context.Context, db *sql.DB, role, schema string, nc *nats.Conn, orch *tcc.Orchestrator, logger *slog.Logger) error {
	subjects := []struct {
		subject string
		handle  func(context.Context, *sql.Tx, besdk.Event) error
	}{
		{"finance.credit.rejected.v1", creditRejectedHandler(orch, logger)},
		{"mdm.customer.created.v1", customerSnapshotHandler()},
		{"mdm.customer.updated.v1", customerSnapshotHandler()},
		{"infra.workflow.task.completed.v1", workflowTaskCompletedHandler()},
		{"crm.opportunity.won.v1", opportunityWonHandler(orch, logger)},
	}

	errCh := make(chan error, len(subjects))
	for _, s := range subjects {
		s := s
		go func() {
			errCh <- besdk.Consume(ctx, nc, db, role, schema, s.subject, s.handle)
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err // 返回 error，不 log.Fatal：一个模块退出进程，同一外壳里的成员全跟着没了
	}
}

// creditRejectedPayload 字段照 erp/finance 的事件契约
// （contracts/events/finance.events.json 的 finance.credit.rejected.v1）。
type creditRejectedPayload struct {
	CustomerID string `json:"customer_id"`
	OrderID    string `json:"order_id"`
	Exposure   string `json:"exposure"`
	Limit      string `json:"limit"`
}

func creditRejectedHandler(_ *tcc.Orchestrator, _ *slog.Logger) func(context.Context, *sql.Tx, besdk.Event) error {
	return func(ctx context.Context, tx *sql.Tx, ev besdk.Event) error {
		var p creditRejectedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		orderID, err := strconv.ParseInt(p.OrderID, 10, 64)
		if err != nil {
			return fmt.Errorf("解析 order_id: %w", err)
		}
		return repo.SuspendOrderTx(ctx, tx, orderID,
			fmt.Sprintf("权威额度超限：已用 %s，额度 %s", p.Exposure, p.Limit))
	}
}

// customerPayload 只取快照要的两个字段。credit_limit 在 updated.v1 里不是
// 必填：没带时是空串，UpsertCustomerSnapshotCreditLimitTx 把它当作"额度不变"。
type customerPayload struct {
	ID          string `json:"id"`
	CreditLimit string `json:"credit_limit"`
}

func customerSnapshotHandler() func(context.Context, *sql.Tx, besdk.Event) error {
	return func(_ context.Context, tx *sql.Tx, ev besdk.Event) error {
		var p customerPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		return repo.UpsertCustomerSnapshotCreditLimitTx(tx, p.ID, p.CreditLimit, ev.Version)
	}
}

// workflowTaskCompletedPayload 字段照抄 infra-workflow 已经真实存在的
// 契约（contracts/events/workflow.events.json 的 task.completed.v1）。
type workflowTaskCompletedPayload struct {
	TaskID          string `json:"task_id"`
	Action          string `json:"action"`
	SourceComponent string `json:"source_component"`
	SourceAggregate string `json:"source_aggregate"`
	SourceID        string `json:"source_id"`
}

// workflowTaskCompletedHandler 只关心自己发起的 sales_order 异常待办——
// infra-workflow 是全平台共用的待办箱，同一个 subject 下会有别的业务
// 组件发起的待办完成事件，source_component/source_aggregate 过滤是
// 必须的，不能假设收到的每一条都是自己的。
//
// ⚠️ 只在 action == "APPROVED" 时恢复订单——这是人在 infra-workflow 的
// "我的待办"里点"同意"，对 exception 任务的业务含义是"确认已经人工
// 处理完毕"（同 workflow.openapi.yaml 对 POST /tasks/{id}/approve 的
// 既有注释：exception 任务的"同意"由发起它的业务组件自己解释）。本组件
// 从不调 infra-workflow 的 CloseTask（那是组件间协议，见 REST 契约顶部
// 警告"人代表业务组件创建/关闭待办等于绕过业务规则"，本组件的角色反过
// 来是"人通过 infra-workflow 的通用待办 UI 关闭"），所以永远不会收到
// action == "RESOLVED"；收到 "REJECTED" 时不做任何自动动作——"驳回"对
// 这类异常任务语义不明确（可能是"我确认这单确实处理不了"），交给人后续
// 走其它路径处理，不在这里猜测。
func workflowTaskCompletedHandler() func(context.Context, *sql.Tx, besdk.Event) error {
	return func(ctx context.Context, tx *sql.Tx, ev besdk.Event) error {
		var p workflowTaskCompletedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		if p.SourceComponent != "erp/sales" || p.SourceAggregate != "sales_order" || p.Action != "APPROVED" {
			return nil
		}
		orderID, err := strconv.ParseInt(p.SourceID, 10, 64)
		if err != nil {
			return fmt.Errorf("解析 source_id: %w", err)
		}
		_, err = repo.ResumeFromExceptionTx(ctx, tx, orderID)
		return err
	}
}

// opportunityWonItemPayload/opportunityWonPayload 字段照 crm/opportunity 的
// 事件契约（contracts/events/opportunity.events.json 的 crm.opportunity.won.v1）。
// 不 import 对方的代码，逐字段照抄；revision 与信封的 version 同值，取信封的；
// expected_close_date 用不到，不取。
type opportunityWonItemPayload struct {
	ProductID       string `json:"product_id"`
	Quantity        string `json:"quantity"`
	QuotedUnitPrice string `json:"quoted_unit_price"`
}

type opportunityWonPayload struct {
	OpportunityID string                      `json:"opportunity_id"`
	CustomerID    string                      `json:"customer_id"`
	Items         []opportunityWonItemPayload `json:"items"`
	OwnerID       string                      `json:"owner_id"`
	DeptPath      string                      `json:"dept_path"`
	Currency      string                      `json:"currency"`
}

// opportunityWonHandler 消费赢单事件、自动转订单（赢单转订单走自动路径；
// 也可以做成人工建单，这是一个可选的分叉点）。真正的建单/确认编排在
// tcc.Orchestrator.HandleOpportunityWon 里，它不用这个 handler 拿到的 tx：
// 订单的落库在它自己的短事务里，网络调用（BatchGet / Reserve）在事务之外，
// 与 ConfirmOrder 的正常路径一样。注意 besdk.Consume 的 inbox 事务在 handler
// 返回之前一直开着，所以转单期间占着一条数据库连接（每个赢单事件至多几次
// 调用超时那么久）。本 handler 永远返回 nil：失败与否由 HandleOpportunityWon
// 自己决定要不要开异常待办，不靠事件重投（besdk.Consume 走 NATS 核心订阅，
// handler 返回 error 只记日志，不会重投）。
func opportunityWonHandler(orch *tcc.Orchestrator, logger *slog.Logger) func(context.Context, *sql.Tx, besdk.Event) error {
	return func(ctx context.Context, _ *sql.Tx, ev besdk.Event) error {
		var p opportunityWonPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		items := make([]tcc.OpportunityWonItem, 0, len(p.Items))
		for _, it := range p.Items {
			items = append(items, tcc.OpportunityWonItem{
				ProductID: it.ProductID, Quantity: it.Quantity, QuotedUnitPrice: it.QuotedUnitPrice,
			})
		}
		orch.HandleOpportunityWon(ctx, tcc.OpportunityWonPayload{
			OpportunityID: p.OpportunityID, CustomerID: p.CustomerID, Items: items,
			OwnerID: p.OwnerID, DeptPath: p.DeptPath, Currency: p.Currency, Revision: ev.Version,
		}, logger)
		return nil
	}
}
