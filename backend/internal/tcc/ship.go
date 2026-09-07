package tcc

import (
	"context"
	"fmt"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-sales/gen/erp/inventory/v1"
)

// ShipOrder 预留转实际出库（调 erp-inventory.ConfirmIssue），只有
// CONFIRMED 才能发货（设计计划 §2.1 状态机）。
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

	conn, closeConn, err := client.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	_, err = conn.ConfirmIssue(ctx, &inventoryv1.ConfirmIssueRequest{
		IdempotencyKey: idempotencyKey + ":confirm-issue", ReservationId: order.ReservationID,
	})
	if err != nil {
		return nil, fmt.Errorf("确认出库失败: %w", err)
	}

	return o.Repo.FinalizeShip(ctx, repo.FinalizeShipInput{IdempotencyKey: idempotencyKey, OrderID: orderID})
}
