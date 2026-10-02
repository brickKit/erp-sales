package tcc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/brickKit/erp-sales/v2/backend/internal/decimal"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
)

// reserveIdemKey 派生库存预留这一步专用的幂等键——与 ConfirmOrder 命令
// 本身的 idempotency_key 分开，这样"确认订单"与"预留库存"两个独立的
// 幂等域不会互相干扰（同一个 ConfirmOrder idempotency_key 理论上不会
// 重复，但派生一个专用键让语义更清楚，也方便日志排障）。
func reserveIdemKey(idempotencyKey string) string { return idempotencyKey + ":reserve" }

// ConfirmOrder 是本组件最难的一段代码：
//
//	① BatchGet 客户            失败 → 直接返回，无需补偿
//	② BatchGet 产品            失败 → 直接返回，无需补偿
//	③ 信用额度校验              超限 → 直接返回，无需补偿（用①拿到的额度
//	                            刷新快照后判，刻意排在 Reserve 之前：超额度
//	                            的单不去占库存）
//	④ Reserve 库存             失败 → 直接返回；超时 → 见 resolveAfterTimeout
//	⑤ 落库 + 发事件（同一事务） 失败 → 补偿：CancelReservation
func (o *Orchestrator) ConfirmOrder(ctx context.Context, orderID, idempotencyKey string, logger *slog.Logger) (*repo.Order, error) {
	// 幂等键先于一切：这个键已经成功确认过这张单，原样返回第一次的结果——
	// 第一次之后订单已是 CONFIRMED，先判状态的话重放永远答"不是草稿"。
	if replayed, err := o.Repo.ReplayCommand(ctx, idempotencyKey, repo.CommandConfirm, orderID); err != nil || replayed != nil {
		return replayed, err
	}
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.Status != repo.StatusDraft {
		return nil, fmt.Errorf("%w: order id=%s status=%s", repo.ErrOrderNotDraft, orderID, order.Status)
	}

	// ① BatchGet 客户
	customer, err := o.fetchActiveCustomer(ctx, order.CustomerID)
	if err != nil {
		return nil, err
	}

	// ② BatchGet 产品
	productIDs := make([]string, 0, len(order.Items))
	for _, it := range order.Items {
		productIDs = append(productIDs, it.ProductID)
	}
	if _, err := o.fetchActiveProducts(ctx, uniqueStrings(productIDs)); err != nil {
		return nil, err
	}

	// ③ 信用额度预判（不再多一次网络调用，排在 Reserve 之前）
	if err := o.checkCredit(ctx, customer, order, logger); err != nil {
		return nil, err
	}

	// ④ Reserve 库存
	invConn, closeInv, err := o.Clients.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	defer closeInv()

	reservationID, err := o.reserve(ctx, invConn, orderID, idempotencyKey, order.Items, logger)
	if err != nil {
		return nil, err // 包含"查询也超时，已写对账队列"的情形——见 reserve 内部注释
	}

	// ⑤ 落库 + 发事件；失败则补偿：CancelReservation
	finalized, err := o.Repo.FinalizeConfirm(ctx, repo.FinalizeConfirmInput{
		IdempotencyKey: idempotencyKey, OrderID: orderID, ReservationID: reservationID,
	})
	if err != nil {
		o.compensateReserve(ctx, invConn, orderID, idempotencyKey, reservationID, logger)
		return nil, err
	}
	return finalized, nil
}

// reserve 发起库存预留，超时就先查预留状态再决定（resolveAfterTimeout），返回真正的
// reservation_id（无论是 Reserve 直接返回的，还是超时后按
// idempotency_key 查出来的）。
func (o *Orchestrator) reserve(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient,
	orderID, idempotencyKey string, items []repo.OrderItem, logger *slog.Logger,
) (string, error) {
	reserveCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()

	idemKey := reserveIdemKey(idempotencyKey)
	resp, err := invConn.Reserve(reserveCtx, &inventoryv1.ReserveRequest{
		IdempotencyKey: idemKey, OrderId: orderID, Items: o.buildReserveItems(items),
	})
	if err == nil {
		return resp.ReservationId, nil
	}
	if !isDeadlineExceeded(err) {
		// 服务端明确拒绝（如 FailedPrecondition：库存不足）——请求已经被
		// 受理并处理完，不是"响应没收到"，不许走状态内省，直接失败返回。
		return "", fmt.Errorf("预留库存失败: %w", err)
	}

	// ⚠️ Reserve 超时：响应没收到，不代表请求没被处理——严禁直接调
	// CancelReservation，必须先 GetReservationStatus。
	logger.Warn("Reserve 超时，转入状态内省", "order_id", orderID, "idempotency_key", idemKey)
	return o.resolveAfterTimeout(ctx, invConn, orderID, idemKey, logger)
}

func (o *Orchestrator) resolveAfterTimeout(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient, orderID, idemKey string, logger *slog.Logger,
) (string, error) {
	statusCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()

	resp, err := invConn.GetReservationStatus(statusCtx, &inventoryv1.GetReservationStatusRequest{IdempotencyKey: idemKey})
	if err != nil {
		if isDeadlineExceeded(err) {
			// 查询也超时：写「待对账」表兜底，不阻塞用户请求。
			if qerr := o.Repo.EnqueueReconciliation(ctx, orderID, "", "Reserve 超时且 GetReservationStatus 也超时，idempotency_key="+idemKey); qerr != nil {
				return "", fmt.Errorf("查询预留状态超时，写对账队列也失败: %w", qerr)
			}
			return "", fmt.Errorf("%w: 预留状态未知，已转入对账队列", errReservePending)
		}
		return "", fmt.Errorf("查询预留状态失败: %w", err)
	}

	switch resp.Status {
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED:
		// 当作成功，继续⑤。
		logger.Info("Reserve 超时但已成功提交", "order_id", orderID, "reservation_id", resp.ReservationId)
		return resp.ReservationId, nil
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED:
		// NOT_FOUND：请求根本没到（或没提交），可以安全重试——这里失败返回，
		// 重试交给调用方：带着同一个 idempotency_key 重新调 ConfirmOrder。
		return "", fmt.Errorf("%w: Reserve 未提交（NOT_FOUND），可安全重试", errReservePending)
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_CANCELLED:
		return "", fmt.Errorf("%w: 预留已被撤销", repo.ErrInvalidArgument)
	default:
		return "", fmt.Errorf("未知的预留状态: %v", resp.Status)
	}
}

// maxCompensationFailures 是补偿连续失败多少次之后挂起订单、开异常待办。
// 与 repo.IncrementCompensationAttemptsTx 里转 SUSPENDED 的阈值相同。
const maxCompensationFailures = 3

// compensationBackoff 是两次补偿重试之间的等待（第 n 次失败后等 n 倍）。
var compensationBackoff = 100 * time.Millisecond

// compensateReserve 是 TCC 的补偿动作：⑤失败后释放④已经成功的预留。
//
// 补偿失败就原地重试（同一个幂等键，CancelReservation 是幂等的），每次
// 失败都记进 compensation_attempts；连续失败 3 次时 repo 把订单转
// SUSPENDED，这里再开一条异常待办交给人，绝不无限重试。补偿成功就把计数
// 清零——计数只表示"连续"失败。
//
// ⚠️ 补偿不跟着用户请求一起放弃：调用方的 ctx 可能已经因为客户端断开而
// 取消，但已经成功的预留必须释放，所以这里用去掉取消信号的 ctx，每次
// 远程调用各自限时。
func (o *Orchestrator) compensateReserve(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient,
	orderID, idempotencyKey, reservationID string, logger *slog.Logger,
) {
	if reservationID == "" {
		// 走到"查询也超时"/"NOT_FOUND"分支时根本没有 reservation_id 可撤销
		// ——不需要补偿，直接返回（对账队列/客户端重试自己会处理）。
		return
	}
	ctx = context.WithoutCancel(ctx)
	failures := 0
	for {
		callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
		_, err := invConn.CancelReservation(callCtx, &inventoryv1.CancelReservationRequest{
			IdempotencyKey: idempotencyKey + ":compensate-cancel", ReservationId: reservationID,
		})
		cancel()
		if err == nil {
			logger.Info("补偿成功：已释放预留", "order_id", orderID, "reservation_id", reservationID, "failures", failures)
			if failures > 0 {
				if rerr := o.Repo.ResetCompensationAttempts(ctx, orderID); rerr != nil {
					logger.Error("补偿失败计数清零出错", "order_id", orderID, "error", rerr)
				}
			}
			return
		}
		failures++
		attempts, incErr := o.Repo.IncrementCompensationAttempts(ctx, orderID)
		logger.Warn("补偿失败：CancelReservation 出错", "order_id", orderID, "reservation_id", reservationID,
			"attempts", attempts, "error", err)
		if incErr != nil {
			logger.Error("累加补偿失败计数出错，停止重试", "order_id", orderID, "error", incErr)
			return
		}
		if attempts >= maxCompensationFailures {
			logger.Error("补偿连续失败，订单已挂起，需要人工释放预留", "order_id", orderID,
				"reservation_id", reservationID, "attempts", attempts, "error", err)
			// 订单此时已是 SUSPENDED：ConfirmOrder 顶部的 status != DRAFT 校验
			// 会拒绝任何后续确认，这条待办的幂等键在同一次挂起里只会用一次。
			o.maybeCreateExceptionTask(ctx, orderID, idempotencyKey, logger)
			return
		}
		time.Sleep(time.Duration(failures) * compensationBackoff)
	}
}

// maybeCreateExceptionTask 把"补偿连续失败 3 次"这件事登记成 infra-workflow
// 的一条 exception 待办。两个独立的"跳过"开关都合法，不是错误：可选依赖
// infra/workflow 没装配（WorkflowConfigured 为 false）、或者
// EXCEPTION_ASSIGNEE_SUB 还没配置（没有组织树可以路由，指派给谁要显式配置，
// 见 tcc.Orchestrator 的字段注释）。
func (o *Orchestrator) maybeCreateExceptionTask(ctx context.Context, orderID, idempotencyKey string, logger *slog.Logger) {
	if !o.Clients.WorkflowConfigured() {
		logger.Info("infra/workflow 未装配，跳过建异常待办", "order_id", orderID)
		return
	}
	if o.ExceptionAssigneeSub == "" {
		logger.Warn("EXCEPTION_ASSIGNEE_SUB 未配置，跳过建异常待办", "order_id", orderID)
		return
	}

	wfConn, closeWf, err := o.Clients.Workflow(ctx)
	if err != nil {
		logger.Error("拨号 infra-workflow 失败，跳过建异常待办", "order_id", orderID, "error", err)
		return
	}
	defer closeWf()

	summaryJSON, err := json.Marshal(map[string]string{"order_id": orderID})
	if err != nil {
		logger.Error("序列化异常待办摘要失败", "order_id", orderID, "error", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	_, err = wfConn.CreateTask(callCtx, &workflowv1.CreateTaskRequest{
		// 派生自本次 ConfirmOrder 命令自己的 idempotency_key——同一个确认
		// 命令重放（网络重试）应该拿到同一条待办；订单从 SUSPENDED 恢复
		// 后如果再次补偿失败 3 次，调用方会带一个全新的 idempotency_key
		// 发起新的 ConfirmOrder，天然产生一条新的待办，不会撞上旧的
		// command_idempotency 记录。
		IdempotencyKey: idempotencyKey + ":exception-task",
		Type:           workflowv1.TaskType_TASK_TYPE_EXCEPTION,
		AssigneeSub:    o.ExceptionAssigneeSub,
		// 只配置了被指派人的 sub，不知道他的部门路径，所以留空。workflow 对
		// 空路径的约定是"只有被指派人本人看得到"：部门子树里的其他人、管理
		// 视图都看不到这条待办。绝不填调用方自己的数据范围值。
		AssigneeDeptPath: "",
		Title:            fmt.Sprintf("订单 %s 补偿连续失败，需要人工介入", orderID),
		SummaryJson:      string(summaryJSON),
		SourceComponent:  "erp/sales",
		SourceAggregate:  "sales_order",
		SourceId:         orderID,
		DeepLink:         "/erp/sales/orders/" + orderID,
	})
	if err != nil {
		logger.Error("建异常待办失败", "order_id", orderID, "error", err)
		return
	}
	logger.Info("已建异常待办", "order_id", orderID)
}

// checkCredit 是确认链的第③步，ConfirmOrder 与赢单转单共用：先用第①步
// BatchGet 拿到的客户（mdm/customer 的权威额度与 version）刷新本地快照，再按
// 快照判"已用额度 + 本单金额"是否超过额度。刷新按 version 单调，事件已经带来
// 更新的值时不覆盖；mdm/customer 给的额度写法不合法时沿用快照（没有快照就是
// 0，任何正金额都被拒）。
func (o *Orchestrator) checkCredit(ctx context.Context, customer *customerv1.Customer, order *repo.Order, logger *slog.Logger) error {
	err := o.Repo.RefreshCustomerSnapshot(ctx, customer.Id, customer.CreditLimit, customer.Version)
	if errors.Is(err, repo.ErrInvalidArgument) {
		logger.Warn("mdm/customer 返回的额度不是合法的十进制数，沿用本地快照",
			"customer_id", customer.Id, "credit_limit", customer.CreditLimit)
	} else if err != nil {
		return fmt.Errorf("刷新信用额度快照失败: %w", err)
	}
	creditLimit, creditExposure, err := o.Repo.GetCustomerSnapshot(ctx, order.CustomerID)
	if err != nil {
		return fmt.Errorf("查信用额度快照失败: %w", err)
	}
	if exceedsCreditLimit(creditExposure, order.TotalAmount, creditLimit) {
		return fmt.Errorf("%w: customer_id=%s 已用 %s + 本单 %s 超过额度 %s",
			repo.ErrInvalidArgument, order.CustomerID, creditExposure, order.TotalAmount, creditLimit)
	}
	return nil
}

// exceedsCreditLimit 判断"已用额度 + 本单金额"是否超过"额度"，十进制
// 精确比较。解析不了的值当作 0：本地查不到客户快照时额度就是 0，任何
// 正金额的订单都会被拒，要等 mdm.customer.* 事件把额度带过来。
func exceedsCreditLimit(exposure, orderAmount, limit string) bool {
	used := new(big.Rat).Add(decimal.ParseOrZero(exposure), decimal.ParseOrZero(orderAmount))
	return used.Cmp(decimal.ParseOrZero(limit)) > 0
}
