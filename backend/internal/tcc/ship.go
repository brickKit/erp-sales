package tcc

import (
	"context"
	"fmt"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
)

// ShipOrder 预留转实际出库（调 erp-inventory.ConfirmIssue），只有
// CONFIRMED 才能发货。
func (o *Orchestrator) ShipOrder(ctx context.Context, orderID, idempotencyKey string) (*repo.Order, error) {
	order, err := o.Repo.GetOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if order.Status != repo.StatusConfirmed {
		return nil, fmt.Errorf("%w: order id=%s status=%s，只有 CONFIRMED 才能发货", repo.ErrOrderNotDraft, orderID, order.Status)
	}
	if order.ReservationID == "" {
		return nil, fmt.Errorf("%w: order id=%s 没有 reservation_id，无法发货", repo.ErrInvalidArgument, orderID)
	}

	conn, closeConn, err := o.Clients.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	// 超时只报错、不补偿：没有需要撤销的本地写，调用方带同一个幂等键重试，
	// ConfirmIssue 按幂等键返回同一结果。
	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	_, err = conn.ConfirmIssue(callCtx, &inventoryv1.ConfirmIssueRequest{
		IdempotencyKey: idempotencyKey + ":confirm-issue", ReservationId: order.ReservationID,
	})
	if err != nil {
		return nil, fmt.Errorf("确认出库失败: %w", err)
	}

	return o.Repo.FinalizeShip(ctx, repo.FinalizeShipInput{IdempotencyKey: idempotencyKey, OrderID: orderID})
}
