package tcc

import (
	"context"
	"fmt"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
)

// CancelOrder 释放预留（如果有）+ 落库 + 发事件。
// DRAFT 从没预留过库存，直接落库；CONFIRMED 需要先调
// erp-inventory.CancelReservation 才能落库；CANCELLED 幂等；其余状态
// 不可取消（CANCELLED 是终态，不能复活，也意味着
// SHIPPED/COMPLETED/CLOSED 之后不能反向取消）。
//
// SUSPENDED 分两种：财务拒绝信用挂起的单是确认过的，占着预留——取消是它唯一
// 的出路，与 CONFIRMED 一样先释放再落库；补偿连续失败挂起的单确认从没提交、
// 没有 reservation_id，它占着的预留只有异常待办的处理人知道，直接取消会让那份
// 预留永远没人管，所以拒绝。
func (o *Orchestrator) CancelOrder(ctx context.Context, orderID, idempotencyKey, reason string) (*repo.Order, error) {
	// 幂等键先于状态校验（见 ConfirmOrder）；这里还挡住"拿确认用过的键来取消"。
	if replayed, err := o.Repo.ReplayCommand(ctx, idempotencyKey, repo.CommandCancel, orderID); err != nil || replayed != nil {
		return replayed, err
	}
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}

	switch order.Status {
	case repo.StatusDraft, repo.StatusCancelled:
		// DRAFT 没有预留可释放；CANCELLED 是幂等重试，FinalizeCancel 自己
		// 处理（如实返回当前状态，不重复发事件）。
	case repo.StatusSuspended:
		if order.SuspendedByCompensation() {
			return nil, fmt.Errorf("%w: order id=%s 因补偿连续失败挂起，预留由异常待办的处理人释放，不能直接取消", repo.ErrOrderNotDraft, orderID)
		}
		if order.ReservationID != "" {
			if err := o.cancelReservation(ctx, order.ReservationID, idempotencyKey); err != nil {
				return nil, err
			}
		}
	case repo.StatusConfirmed:
		if order.ReservationID != "" {
			if err := o.cancelReservation(ctx, order.ReservationID, idempotencyKey); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("%w: order id=%s status=%s 不能取消", repo.ErrOrderTerminal, orderID, order.Status)
	}

	return o.Repo.FinalizeCancel(ctx, repo.FinalizeCancelInput{
		IdempotencyKey: idempotencyKey, OrderID: orderID, Reason: reason,
	})
}

func (o *Orchestrator) cancelReservation(ctx context.Context, reservationID, idempotencyKey string) error {
	conn, closeConn, err := o.Clients.Inventory(ctx)
	if err != nil {
		return err
	}
	defer closeConn()

	// 超时只报错：订单仍是 CONFIRMED，调用方带同一个幂等键重试即可。
	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	_, err = conn.CancelReservation(callCtx, &inventoryv1.CancelReservationRequest{
		IdempotencyKey: idempotencyKey + ":cancel-reservation", ReservationId: reservationID,
	})
	if err != nil {
		return fmt.Errorf("释放库存预留失败: %w", err)
	}
	return nil
}
