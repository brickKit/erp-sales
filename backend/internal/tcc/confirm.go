package tcc

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-sales/gen/erp/inventory/v1"
)

// reserveIdemKey 派生库存预留这一步专用的幂等键——与 ConfirmOrder 命令
// 本身的 idempotency_key 分开，这样"确认订单"与"预留库存"两个独立的
// 幂等域不会互相干扰（同一个 ConfirmOrder idempotency_key 理论上不会
// 重复，但派生一个专用键让语义更清楚，也方便日志排障）。
func reserveIdemKey(idempotencyKey string) string { return idempotencyKey + ":reserve" }

// ConfirmOrder 是本阶段最难的一段代码（设计计划 §3.1）：
//
//	① BatchGet 客户            失败 → 直接返回，无需补偿
//	② BatchGet 产品            失败 → 直接返回，无需补偿
//	③ 本地缓存校验信用额度      超限 → 直接返回，无需补偿（零网络开销，
//	                            刻意排在网络调用之前——§3.1 与设计书 §8.2
//	                            顺序不同的地方，出档时回填 §8.2）
//	④ Reserve 库存             失败 → 直接返回；超时 → 见 resolveAfterTimeout
//	⑤ 落库 + 发事件（同一事务） 失败 → 补偿：CancelReservation
func (o *Orchestrator) ConfirmOrder(ctx context.Context, orderID, idempotencyKey string, logger *slog.Logger) (*repo.Order, error) {
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.Status != repo.StatusDraft {
		return nil, fmt.Errorf("%w: order id=%s status=%s", repo.ErrOrderNotDraft, orderID, order.Status)
	}

	// ① BatchGet 客户
	if _, err := fetchActiveCustomer(ctx, order.CustomerID); err != nil {
		return nil, err
	}

	// ② BatchGet 产品
	productIDs := make([]string, 0, len(order.Items))
	for _, it := range order.Items {
		productIDs = append(productIDs, it.ProductID)
	}
	if _, err := fetchActiveProducts(ctx, uniqueStrings(productIDs)); err != nil {
		return nil, err
	}

	// ③ 本地信用额度预判（零网络开销，排在 Reserve 之前——设计计划 §3.1）
	creditLimit, creditExposure, err := o.Repo.GetCustomerSnapshot(ctx, order.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("查信用额度快照失败: %w", err)
	}
	if exceedsCreditLimit(creditExposure, order.TotalAmount, creditLimit) {
		return nil, fmt.Errorf("%w: customer_id=%s 已用 %s + 本单 %s 超过额度 %s",
			repo.ErrInvalidArgument, order.CustomerID, creditExposure, order.TotalAmount, creditLimit)
	}

	// ④ Reserve 库存
	invConn, closeInv, err := client.Inventory(ctx)
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

// reserve 发起库存预留，超时按 §4.5 的状态内省流程处理，返回真正的
// reservation_id（无论是 Reserve 直接返回的，还是超时后按
// idempotency_key 查出来的）。
func (o *Orchestrator) reserve(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient,
	orderID, idempotencyKey string, items []repo.OrderItem, logger *slog.Logger,
) (string, error) {
	reserveCtx, cancel := context.WithTimeout(ctx, o.ReserveTimeout)
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
	// CancelReservation，必须先 GetReservationStatus（设计计划 §4.5）。
	logger.Warn("Reserve 超时，转入状态内省", "order_id", orderID, "idempotency_key", idemKey)
	return o.resolveAfterTimeout(ctx, invConn, orderID, idemKey, logger)
}

func (o *Orchestrator) resolveAfterTimeout(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient, orderID, idemKey string, logger *slog.Logger,
) (string, error) {
	statusCtx, cancel := context.WithTimeout(ctx, o.ReserveTimeout)
	defer cancel()

	resp, err := invConn.GetReservationStatus(statusCtx, &inventoryv1.GetReservationStatusRequest{IdempotencyKey: idemKey})
	if err != nil {
		if isDeadlineExceeded(err) {
			// 查询也超时：写「待对账」表兜底，不阻塞用户请求（§4.5、§9 第 8 条）。
			if qerr := o.Repo.EnqueueReconciliation(ctx, orderID, "", "Reserve 超时且 GetReservationStatus 也超时，idempotency_key="+idemKey); qerr != nil {
				return "", fmt.Errorf("查询预留状态超时，写对账队列也失败: %w", qerr)
			}
			return "", fmt.Errorf("%w: 预留状态未知，已转入对账队列", errReservePending)
		}
		return "", fmt.Errorf("查询预留状态失败: %w", err)
	}

	switch resp.Status {
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED:
		// 当作成功，继续⑤（设计计划 §4.5）。
		logger.Info("Reserve 超时但已成功提交", "order_id", orderID, "reservation_id", resp.ReservationId)
		return resp.ReservationId, nil
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED:
		// NOT_FOUND：请求根本没到（或没提交），可以安全重试或失败返回——
		// 阶段二选失败返回，重试交给上游调用方（同 CreateOrder 的幂等键，
		// 客户端可以带着同一个 idempotency_key 重新调 ConfirmOrder）。
		return "", fmt.Errorf("%w: Reserve 未提交（NOT_FOUND），可安全重试", errReservePending)
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_CANCELLED:
		return "", fmt.Errorf("%w: 预留已被撤销", repo.ErrInvalidArgument)
	default:
		return "", fmt.Errorf("未知的预留状态: %v", resp.Status)
	}
}

// compensateReserve 是 TCC 的补偿动作：⑤失败后释放④已经成功的预留。
// 连续失败次数由 repo.IncrementCompensationAttemptsTx 累加，3 次后自动
// 转 SUSPENDED（设计计划 §3.1、§4.4.4）——阶段二只标状态+打日志，绝不
// 无限重试。
func (o *Orchestrator) compensateReserve(
	ctx context.Context, invConn inventoryv1.InventoryServiceClient,
	orderID, idempotencyKey, reservationID string, logger *slog.Logger,
) {
	if reservationID == "" {
		// 走到"查询也超时"/"NOT_FOUND"分支时根本没有 reservation_id 可撤销
		// ——不需要补偿，直接返回（对账队列/客户端重试自己会处理）。
		return
	}
	_, err := invConn.CancelReservation(ctx, &inventoryv1.CancelReservationRequest{
		IdempotencyKey: idempotencyKey + ":compensate-cancel", ReservationId: reservationID,
	})
	attempts, incErr := o.Repo.IncrementCompensationAttempts(ctx, orderID)
	if err != nil {
		logger.Error("补偿失败：CancelReservation 出错", "order_id", orderID, "reservation_id", reservationID,
			"attempts", attempts, "error", err)
	} else {
		logger.Info("补偿成功：已释放预留", "order_id", orderID, "reservation_id", reservationID)
	}
	if incErr != nil {
		logger.Error("累加补偿失败计数出错", "order_id", orderID, "error", incErr)
	}
}

// exceedsCreditLimit 判断"已用额度 + 本单金额"是否超过"额度"。三个都是
// decimal-as-string（§附录：金额字段一律 string 传 decimal）。
func exceedsCreditLimit(exposure, orderAmount, limit string) bool {
	e, _ := strconv.ParseFloat(exposure, 64)
	a, _ := strconv.ParseFloat(orderAmount, 64)
	l, _ := strconv.ParseFloat(limit, 64)
	return e+a > l
}
