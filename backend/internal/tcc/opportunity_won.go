// 阶段三 Task 14：crm.opportunity.won.v1 的消费入口——赢单转订单是本
// 阶段选定的自动路径（设计计划 §8 判定为 Fork 点，本阶段选自动；
// crm-opportunity 设计计划 §6/§4.1、附录 E）。
//
// ⚠️ 这段代码跑在事件 handler 里，没有 JWT 可透传（§14.2.6）：归属信息
// （owner_id/dept_path）与身份（SystemClient）都必须来自事件本身，不能
// 走 besdk.ScopeOf(ctx)/UserClient。这也是为什么本文件不直接复用
// CreateOrder/ConfirmOrder 两个公开方法——它们内部硬编码了 UserClient
// 拨号与 ScopeOf(ctx) 取值，两者在事件 handler 语境下都不成立。真正
// 微妙的部分（Reserve 超时状态内省、补偿、claim-first 幂等）完全复用
// 同一套已经验证过的 o.reserve/o.compensateReserve/o.Repo.* 辅助函数，
// 本文件只是外层编排换了一套"客户端从哪拨、归属从哪来"。
package tcc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	besdk "github.com/brickKit/be-sdk-go"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	workflowv1 "github.com/brickKit/erp-sales/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/erp-sales/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/erp-sales/gen/mdm/product/v1"
)

// OpportunityWonItem 对应 crm.opportunity.won.v1 payload 的 items[] 一项。
// ⚠️ QuotedUnitPrice 是报价快照供参考，不是权威价格——本组件用自己的
// 定价引擎（CalculatePriceDryRun）重算，两者允许不一致（crm-opportunity
// 设计计划 §2.1，本组件契约注释同一句话已经写过一次）。
type OpportunityWonItem struct {
	ProductID       string
	Quantity        string
	QuotedUnitPrice string
}

// OpportunityWonPayload 对应 crm.opportunity.won.v1 的完整 payload
// （crm-opportunity 设计计划 §4.1 的字段表）。⭐ OwnerID/DeptPath 是
// 最容易漏的两个字段——这条事件 handler 唯一能拿到归属信息的地方。
type OpportunityWonPayload struct {
	OpportunityID string
	CustomerID    string
	Items         []OpportunityWonItem
	OwnerID       string
	DeptPath      string
	Currency      string
	Revision      int64
}

// HandleOpportunityWon 是本文件唯一的导出入口。⚠️ 不返回 error 给调用方
// 重试——crm-opportunity 已经真实赢单，这条链路是"尽力而为"地转成订单；
// 任何一步失败都建一条 infra-workflow 异常待办通知事件里带的 owner_id，
// 不通过事件回传失败给 crm-opportunity（crm-opportunity 设计计划 §9
// 待决问题 2：那会让 CRM 反向依赖 ERP 状态机，商机的 WON 状态永远不因为
// ERP 侧失败而回滚）。
func (o *Orchestrator) HandleOpportunityWon(ctx context.Context, p OpportunityWonPayload, logger *slog.Logger) {
	order, err := o.createOrderForOpportunity(ctx, p)
	if err != nil {
		logger.Error("商机赢单自动建单失败", "opportunity_id", p.OpportunityID, "error", err)
		o.createOpportunityExceptionTask(ctx, p, "建单失败: "+err.Error(), logger)
		return
	}

	idemKey := "crm-won:" + p.OpportunityID + ":confirm"
	if _, err := o.confirmOrderForOpportunity(ctx, order.ID, idemKey, logger); err != nil {
		logger.Error("商机赢单自动确认订单失败", "opportunity_id", p.OpportunityID, "order_id", order.ID, "error", err)
		o.createOpportunityExceptionTask(ctx, p, fmt.Sprintf("订单 %s 确认失败: %s", order.ID, err.Error()), logger)
		return
	}

	logger.Info("商机赢单自动建单确认成功", "opportunity_id", p.OpportunityID, "order_id", order.ID)
}

// fetchActiveCustomerSystem/fetchActiveProductsSystem 同 tcc.go 的
// fetchActiveCustomer/fetchActiveProducts，唯一区别是用 client.*System
// 系统身份拨号（见 client.go 的 dialSystem 注释）。
func fetchActiveCustomerSystem(ctx context.Context, customerID string) (*customerv1.Customer, error) {
	conn, closeConn, err := client.CustomerSystem()
	if err != nil {
		return nil, err
	}
	defer closeConn()

	resp, err := conn.BatchGet(ctx, &customerv1.BatchGetRequest{Ids: []string{customerID}})
	if err != nil {
		return nil, fmt.Errorf("校验客户失败: %w", err)
	}
	if len(resp.Customers) == 0 {
		return nil, fmt.Errorf("%w: 客户不存在：customer_id=%s", repo.ErrInvalidArgument, customerID)
	}
	c := resp.Customers[0]
	if c.Status != customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE {
		return nil, fmt.Errorf("%w: 客户不可用：customer_id=%s status=%s", repo.ErrInvalidArgument, customerID, c.Status)
	}
	return c, nil
}

func fetchActiveProductsSystem(ctx context.Context, productIDs []string) (map[string]*productv1.Product, error) {
	conn, closeConn, err := client.ProductSystem()
	if err != nil {
		return nil, err
	}
	defer closeConn()

	resp, err := conn.BatchGet(ctx, &productv1.BatchGetRequest{Ids: productIDs})
	if err != nil {
		return nil, fmt.Errorf("校验产品失败: %w", err)
	}
	if len(resp.MissingIds) > 0 {
		return nil, fmt.Errorf("%w: 产品不存在：%v", repo.ErrInvalidArgument, resp.MissingIds)
	}
	byID := make(map[string]*productv1.Product, len(resp.Products))
	for _, p := range resp.Products {
		if p.Status != productv1.ProductStatus_PRODUCT_STATUS_ACTIVE {
			return nil, fmt.Errorf("%w: 产品不可用：product_id=%s status=%s", repo.ErrInvalidArgument, p.Id, p.Status)
		}
		byID[p.Id] = p
	}
	return byID, nil
}

// createOrderForOpportunity 镜像 CreateOrder（create.go）：校验客户/产品、
// 本地定价、落库，唯一区别是客户端走系统身份、归属字段来自事件而不是
// besdk.ScopeOf(ctx)。
func (o *Orchestrator) createOrderForOpportunity(ctx context.Context, p OpportunityWonPayload) (*repo.Order, error) {
	if len(p.Items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", repo.ErrInvalidArgument)
	}

	customer, err := fetchActiveCustomerSystem(ctx, p.CustomerID)
	if err != nil {
		return nil, err
	}

	productIDs := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		productIDs = append(productIDs, it.ProductID)
	}
	products, err := fetchActiveProductsSystem(ctx, uniqueStrings(productIDs))
	if err != nil {
		return nil, err
	}

	priceInputs := make([]repo.PriceInput, 0, len(p.Items))
	for _, it := range p.Items {
		prod := products[it.ProductID]
		priceInputs = append(priceInputs, repo.PriceInput{ProductID: it.ProductID, CategoryID: prod.CategoryId, Qty: it.Quantity})
	}
	priceResults, _, err := o.Repo.CalculatePriceDryRun(ctx, priceInputs)
	if err != nil {
		return nil, fmt.Errorf("定价失败: %w", err)
	}
	if len(priceResults) != len(p.Items) {
		return nil, fmt.Errorf("定价结果数量（%d）与商机行数量（%d）不一致", len(priceResults), len(p.Items))
	}

	createItems := make([]repo.CreateOrderItemInput, 0, len(p.Items))
	for i, it := range p.Items {
		prod := products[it.ProductID]
		pr := priceResults[i]
		// ⚠️ pr.UnitPrice 是重算出来的权威价格，it.QuotedUnitPrice 只是
		// 商机行的报价快照——两者允许不一致，只记日志供人工核对差异，
		// 不做任何自动比对/告警（不在 Task 14 的验证标准内，属于未来
		// 可能需要的价格审计功能，YAGNI）。
		createItems = append(createItems, repo.CreateOrderItemInput{
			ProductID: it.ProductID, ProductSKU: prod.Sku, ProductName: prod.Name, UOMID: prod.BaseUomId,
			Qty: it.Quantity, UnitPrice: pr.UnitPrice, Discount: pr.Discount, TaxRate: pr.TaxRate, Subtotal: pr.Subtotal,
		})
	}

	return o.Repo.CreateOrder(ctx, repo.CreateOrderInput{
		// 幂等键必须从 opportunity_id 确定性派生——同一个商机的赢单事件
		// 只应该产生一个 DRAFT 订单，重复消费（若未来事件总线支持重投）
		// 必须落到同一条 command_idempotency 记录上。
		IdempotencyKey: "crm-won:" + p.OpportunityID, CustomerID: p.CustomerID, CustomerName: customer.Name,
		Items: createItems, DeptID: leafDeptID(p.DeptPath), DeptPath: p.DeptPath, OwnerID: p.OwnerID,
	})
}

// confirmOrderForOpportunity 镜像 ConfirmOrder（confirm.go）的编排壳，
// Reserve 超时状态内省/补偿等真正微妙的逻辑完全复用 o.reserve/
// o.compensateReserve——唯一区别同样是客户端走系统身份。
func (o *Orchestrator) confirmOrderForOpportunity(ctx context.Context, orderID, idempotencyKey string, logger *slog.Logger) (*repo.Order, error) {
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.Status != repo.StatusDraft {
		return nil, fmt.Errorf("%w: order id=%s status=%s", repo.ErrOrderNotDraft, orderID, order.Status)
	}

	if _, err := fetchActiveCustomerSystem(ctx, order.CustomerID); err != nil {
		return nil, err
	}
	productIDs := make([]string, 0, len(order.Items))
	for _, it := range order.Items {
		productIDs = append(productIDs, it.ProductID)
	}
	if _, err := fetchActiveProductsSystem(ctx, uniqueStrings(productIDs)); err != nil {
		return nil, err
	}

	creditLimit, creditExposure, err := o.Repo.GetCustomerSnapshot(ctx, order.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("查信用额度快照失败: %w", err)
	}
	if exceedsCreditLimit(creditExposure, order.TotalAmount, creditLimit) {
		return nil, fmt.Errorf("%w: customer_id=%s 已用 %s + 本单 %s 超过额度 %s",
			repo.ErrInvalidArgument, order.CustomerID, creditExposure, order.TotalAmount, creditLimit)
	}

	invConn, closeInv, err := client.InventorySystem()
	if err != nil {
		return nil, err
	}
	defer closeInv()

	reservationID, err := o.reserve(ctx, invConn, orderID, idempotencyKey, order.Items, logger)
	if err != nil {
		return nil, err
	}

	finalized, err := o.Repo.FinalizeConfirm(ctx, repo.FinalizeConfirmInput{
		IdempotencyKey: idempotencyKey, OrderID: orderID, ReservationID: reservationID,
	})
	if err != nil {
		o.compensateReserve(ctx, invConn, orderID, idempotencyKey, reservationID, logger)
		return nil, err
	}
	return finalized, nil
}

// createOpportunityExceptionTask 建一条 infra-workflow exception 待办，
// assignee 用事件里带的 owner_id——正好是 crm-opportunity 设计计划 §4.1
// 里 owner_id 字段的第二个用途（crm-opportunity 设计计划 §9 待决问题
// 2）。⚠️ 这条待办与 compensateReserve→maybeCreateExceptionTask 那条
// （补偿连续失败 3 次、assignee 是静态配置的 exceptionAssigneeSub）是
// 两条独立的路径：本函数覆盖的是"建单/确认这条自动链路本身失败"（可能
// 根本没有 Reserve 成功过，比如库存不足直接被拒），不要求补偿失败过。
func (o *Orchestrator) createOpportunityExceptionTask(ctx context.Context, p OpportunityWonPayload, reason string, logger *slog.Logger) {
	if _, ok := besdk.Endpoint("infra/workflow", "grpc"); !ok {
		logger.Info("infra/workflow 未装配，跳过建异常待办", "opportunity_id", p.OpportunityID)
		return
	}

	wfConn, closeWf, err := client.WorkflowSystem()
	if err != nil {
		logger.Error("拨号 infra-workflow 失败，跳过建异常待办", "opportunity_id", p.OpportunityID, "error", err)
		return
	}
	defer closeWf()

	summaryJSON, err := json.Marshal(map[string]string{"opportunity_id": p.OpportunityID, "reason": reason})
	if err != nil {
		logger.Error("序列化异常待办摘要失败", "opportunity_id", p.OpportunityID, "error", err)
		return
	}
	_, err = wfConn.CreateTask(ctx, &workflowv1.CreateTaskRequest{
		IdempotencyKey:   "crm-won:" + p.OpportunityID + ":exception-task",
		Type:             workflowv1.TaskType_TASK_TYPE_EXCEPTION,
		AssigneeSub:      p.OwnerID,
		AssigneeDeptPath: p.DeptPath,
		Title:            fmt.Sprintf("商机 %s 赢单自动转订单失败，需要人工介入", p.OpportunityID),
		SummaryJson:      string(summaryJSON),
		SourceComponent:  "erp/sales",
		SourceAggregate:  "opportunity_conversion",
		SourceId:         p.OpportunityID,
		DeepLink:         "/crm/opportunity/opportunities/" + p.OpportunityID,
	})
	if err != nil {
		logger.Error("建异常待办失败", "opportunity_id", p.OpportunityID, "error", err)
		return
	}
	logger.Info("已建异常待办", "opportunity_id", p.OpportunityID, "assignee_sub", p.OwnerID)
}
