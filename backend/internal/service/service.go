// Package service 是 erp-sales 的业务规则层：入参校验 + 给 http/grpc
// 一个不依赖 repo/tcc 内部细节的稳定入口（同 erp-finance/erp-inventory
// 的判据）。ConfirmOrder 真正的 TCC 编排在 backend/internal/tcc，这一层
// 只负责校验 + 转调 + 记日志。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/tcc"
)

var ErrInvalidArgument = errors.New("参数不合法")

type Service struct {
	repo   *repo.Repo
	tcc    *tcc.Orchestrator
	logger *slog.Logger
}

func New(r *repo.Repo, orch *tcc.Orchestrator, logger *slog.Logger) *Service {
	return &Service{repo: r, tcc: orch, logger: logger}
}

// ── 命令：CreateOrder/ConfirmOrder/CancelOrder/ShipOrder ──

type CreateOrderItem struct {
	ProductID string
	Qty       string
}

func (s *Service) CreateOrder(ctx context.Context, idempotencyKey, customerID string, items []CreateOrderItem) (*repo.Order, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if customerID == "" {
		return nil, fmt.Errorf("%w: customer_id 不能为空", ErrInvalidArgument)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	in := make([]tcc.CreateOrderItemInput, 0, len(items))
	for _, it := range items {
		if it.ProductID == "" || it.Qty == "" {
			return nil, fmt.Errorf("%w: product_id/qty 不能为空", ErrInvalidArgument)
		}
		in = append(in, tcc.CreateOrderItemInput{ProductID: it.ProductID, Qty: it.Qty})
	}
	order, err := s.tcc.CreateOrder(ctx, idempotencyKey, customerID, in)
	if err != nil {
		s.logger.Error("建单失败", "customer_id", customerID, "error", err)
		return nil, err
	}
	return order, nil
}

func (s *Service) ConfirmOrder(ctx context.Context, orderID, idempotencyKey string) (*repo.Order, error) {
	if orderID == "" {
		return nil, fmt.Errorf("%w: order_id 不能为空", ErrInvalidArgument)
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	order, err := s.tcc.ConfirmOrder(ctx, orderID, idempotencyKey, s.logger)
	if err != nil {
		s.logger.Error("确认订单失败", "order_id", orderID, "error", err)
		return nil, err
	}
	return order, nil
}

func (s *Service) CancelOrder(ctx context.Context, orderID, idempotencyKey, reason string) (*repo.Order, error) {
	if orderID == "" {
		return nil, fmt.Errorf("%w: order_id 不能为空", ErrInvalidArgument)
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	order, err := s.tcc.CancelOrder(ctx, orderID, idempotencyKey, reason)
	if err != nil {
		s.logger.Error("取消订单失败", "order_id", orderID, "error", err)
		return nil, err
	}
	return order, nil
}

func (s *Service) ShipOrder(ctx context.Context, orderID, idempotencyKey string) (*repo.Order, error) {
	if orderID == "" {
		return nil, fmt.Errorf("%w: order_id 不能为空", ErrInvalidArgument)
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	order, err := s.tcc.ShipOrder(ctx, orderID, idempotencyKey)
	if err != nil {
		s.logger.Error("发货失败", "order_id", orderID, "error", err)
		return nil, err
	}
	return order, nil
}

// ── 定价 ──

func (s *Service) CalculatePriceDryRun(ctx context.Context, items []repo.PriceInput) ([]*repo.PriceResult, string, error) {
	if len(items) == 0 {
		return nil, "", fmt.Errorf("%w: items 不能为空", ErrInvalidArgument)
	}
	return s.repo.CalculatePriceDryRun(ctx, items)
}

// ── 读 ──

func (s *Service) GetOrder(ctx context.Context, id string) (*repo.Order, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id 不能为空", ErrInvalidArgument)
	}
	return s.repo.GetOrder(ctx, id)
}

func (s *Service) ListOrders(ctx context.Context, in repo.ListInput) (*repo.ListResult, error) {
	return s.repo.ListOrders(ctx, in)
}

func (s *Service) BatchGetOrder(ctx context.Context, ids []string) (found []*repo.Order, missing []string, err error) {
	return s.repo.BatchGetOrder(ctx, ids)
}

func (s *Service) GetOrderStatus(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: id 不能为空", ErrInvalidArgument)
	}
	return s.repo.GetOrderStatus(ctx, id)
}
