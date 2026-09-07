// Package grpc 实现 erp.sales.v1.SalesService——内部 gRPC 面（§2.1）。
// HTTP 与 gRPC 共用同一个 service.Service，业务逻辑只写一遍。
package grpc

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"

	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/service"
)

type server struct {
	salesv1.UnimplementedSalesServiceServer
	svc *service.Service
}

func New(svc *service.Service) salesv1.SalesServiceServer {
	return &server{svc: svc}
}

func toProtoStatus(s string) salesv1.OrderStatus {
	switch s {
	case repo.StatusDraft:
		return salesv1.OrderStatus_ORDER_STATUS_DRAFT
	case repo.StatusConfirmed:
		return salesv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case repo.StatusShipped:
		return salesv1.OrderStatus_ORDER_STATUS_SHIPPED
	case repo.StatusCompleted:
		return salesv1.OrderStatus_ORDER_STATUS_COMPLETED
	case repo.StatusCancelled:
		return salesv1.OrderStatus_ORDER_STATUS_CANCELLED
	case repo.StatusClosed:
		return salesv1.OrderStatus_ORDER_STATUS_CLOSED
	case repo.StatusSuspended:
		return salesv1.OrderStatus_ORDER_STATUS_SUSPENDED
	default:
		return salesv1.OrderStatus_ORDER_STATUS_UNSPECIFIED
	}
}

// fromProtoStatus 是 toProtoStatus 的逆映射，ListOrders 的 status_filter
// 用——不能用 pb 枚举的 .String()（那会得到 "ORDER_STATUS_CONFIRMED"
// 这种带前缀的名字，repo 层的状态常量是不带前缀的裸值 "CONFIRMED"）。
func fromProtoStatus(s salesv1.OrderStatus) string {
	switch s {
	case salesv1.OrderStatus_ORDER_STATUS_DRAFT:
		return repo.StatusDraft
	case salesv1.OrderStatus_ORDER_STATUS_CONFIRMED:
		return repo.StatusConfirmed
	case salesv1.OrderStatus_ORDER_STATUS_SHIPPED:
		return repo.StatusShipped
	case salesv1.OrderStatus_ORDER_STATUS_COMPLETED:
		return repo.StatusCompleted
	case salesv1.OrderStatus_ORDER_STATUS_CANCELLED:
		return repo.StatusCancelled
	case salesv1.OrderStatus_ORDER_STATUS_CLOSED:
		return repo.StatusClosed
	case salesv1.OrderStatus_ORDER_STATUS_SUSPENDED:
		return repo.StatusSuspended
	default:
		return ""
	}
}

func toProtoOrder(o *repo.Order) *salesv1.Order {
	items := make([]*salesv1.OrderItem, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, &salesv1.OrderItem{
			ProductId: it.ProductID, ProductSku: it.ProductSKU, ProductName: it.ProductName,
			UomId: it.UOMID, Qty: it.Qty, UnitPrice: it.UnitPrice, Discount: it.Discount,
			TaxRate: it.TaxRate, Subtotal: it.Subtotal,
		})
	}
	return &salesv1.Order{
		Id: o.ID, OrderNo: o.OrderNo, CustomerId: o.CustomerID, CustomerName: o.CustomerName,
		Status: toProtoStatus(o.Status), Items: items, TotalAmount: o.TotalAmount,
		DeptId: o.DeptID, DeptPath: o.DeptPath, OwnerId: o.OwnerID, ReservationId: o.ReservationID,
		Version: o.Version, CreatedAt: timestamppb.New(o.CreatedAt), UpdatedAt: timestamppb.New(o.UpdatedAt),
	}
}

func (s *server) CreateOrder(ctx context.Context, req *salesv1.CreateOrderRequest) (*salesv1.CreateOrderResponse, error) {
	items := make([]service.CreateOrderItem, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, service.CreateOrderItem{ProductID: it.ProductId, Qty: it.Qty})
	}
	order, err := s.svc.CreateOrder(ctx, req.IdempotencyKey, req.CustomerId, items)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &salesv1.CreateOrderResponse{Order: toProtoOrder(order)}, nil
}

func (s *server) ConfirmOrder(ctx context.Context, req *salesv1.ConfirmOrderRequest) (*salesv1.ConfirmOrderResponse, error) {
	order, err := s.svc.ConfirmOrder(ctx, req.OrderId, req.IdempotencyKey)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &salesv1.ConfirmOrderResponse{Order: toProtoOrder(order)}, nil
}

func (s *server) CancelOrder(ctx context.Context, req *salesv1.CancelOrderRequest) (*salesv1.CancelOrderResponse, error) {
	order, err := s.svc.CancelOrder(ctx, req.OrderId, req.IdempotencyKey, req.Reason)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &salesv1.CancelOrderResponse{Order: toProtoOrder(order)}, nil
}

func (s *server) ShipOrder(ctx context.Context, req *salesv1.ShipOrderRequest) (*salesv1.ShipOrderResponse, error) {
	order, err := s.svc.ShipOrder(ctx, req.OrderId, req.IdempotencyKey)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &salesv1.ShipOrderResponse{Order: toProtoOrder(order)}, nil
}

func (s *server) CalculatePriceDryRun(ctx context.Context, req *salesv1.CalculatePriceDryRunRequest) (*salesv1.CalculatePriceDryRunResponse, error) {
	in := make([]repo.PriceInput, 0, len(req.Items))
	for _, it := range req.Items {
		in = append(in, repo.PriceInput{ProductID: it.ProductId, Qty: it.Qty})
	}
	results, total, err := s.svc.CalculatePriceDryRun(ctx, in)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	out := make([]*salesv1.PriceDryRunResultItem, 0, len(results))
	for _, r := range results {
		out = append(out, &salesv1.PriceDryRunResultItem{
			ProductId: r.ProductID, UnitPrice: r.UnitPrice, Discount: r.Discount, TaxRate: r.TaxRate, Subtotal: r.Subtotal,
		})
	}
	return &salesv1.CalculatePriceDryRunResponse{Items: out, TotalAmount: total}, nil
}

func (s *server) GetOrder(ctx context.Context, req *salesv1.GetOrderRequest) (*salesv1.Order, error) {
	order, err := s.svc.GetOrder(ctx, req.Id)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return toProtoOrder(order), nil
}

func (s *server) ListOrders(ctx context.Context, req *salesv1.ListOrdersRequest) (*salesv1.ListOrdersResponse, error) {
	in := repo.ListInput{Cursor: req.Cursor, PageSize: int(req.PageSize), CustomerID: req.CustomerId}
	if req.StatusFilter != salesv1.OrderStatus_ORDER_STATUS_UNSPECIFIED {
		in.StatusFilter = fromProtoStatus(req.StatusFilter)
	}
	if req.CreatedAfter != nil {
		in.CreatedAfter = req.CreatedAfter.AsTime()
	}
	if req.CreatedBefore != nil {
		in.CreatedBefore = req.CreatedBefore.AsTime()
	}
	out, err := s.svc.ListOrders(ctx, in)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	orders := make([]*salesv1.Order, 0, len(out.Orders))
	for _, o := range out.Orders {
		orders = append(orders, toProtoOrder(o))
	}
	return &salesv1.ListOrdersResponse{Orders: orders, NextCursor: out.NextCursor}, nil
}

// BatchGetOrder 是防 N+1 的唯一合法调用方式（§3.8）。
func (s *server) BatchGetOrder(ctx context.Context, req *salesv1.BatchGetOrderRequest) (*salesv1.BatchGetOrderResponse, error) {
	found, missing, err := s.svc.BatchGetOrder(ctx, req.Ids)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	orders := make([]*salesv1.Order, 0, len(found))
	for _, o := range found {
		orders = append(orders, toProtoOrder(o))
	}
	return &salesv1.BatchGetOrderResponse{Orders: orders, MissingIds: missing}, nil
}

// GetOrderStatus 给上游防"薛定谔的超时"用（§4.5）。UNSPECIFIED 就是
// NOT_FOUND 的信号。
func (s *server) GetOrderStatus(ctx context.Context, req *salesv1.GetOrderStatusRequest) (*salesv1.GetOrderStatusResponse, error) {
	status, err := s.svc.GetOrderStatus(ctx, req.OrderId)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &salesv1.GetOrderStatusResponse{Status: toProtoStatus(status)}, nil
}
