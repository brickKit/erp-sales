package tcc

import (
	"context"
	"fmt"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
)

// CancelOrder 释放预留（如果有）+ 落库 + 发事件（设计计划 §3 契约面）。
// DRAFT 从没预留过库存，直接落库；CONFIRMED 需要先调
// erp-inventory.CancelReservation 才能落库；CANCELLED 幂等；其余状态
// 不可取消（设计计划 §2.1：CANCELLED 是终态，不能复活，也意味着
// SHIPPED/COMPLETED/CLOSED 之后不能反向取消）。
func (o *Orchestrator) CancelOrder(ctx context.Context, orderID, idempotencyKey, reason string) (*repo.Order, error) {
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}

	switch order.Status {
	case repo.StatusDraft, repo.StatusCancelled:
		// DRAFT 没有预留可释放；CANCELLED 是幂等重试，FinalizeCancel 自己
		// 处理（如实返回当前状态，不重复发事件）。
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
	conn, closeConn, err := client.Inventory(ctx)
	if err != nil {
		return err
	}
	defer closeConn()

	_, err = conn.CancelReservation(ctx, &inventoryv1.CancelReservationRequest{
		IdempotencyKey: idempotencyKey + ":cancel-reservation", ReservationId: reservationID,
	})
	if err != nil {
		return fmt.Errorf("释放库存预留失败: %w", err)
	}
	return nil
}
