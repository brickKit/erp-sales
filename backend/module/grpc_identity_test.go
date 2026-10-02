package module

import (
	"context"
	"testing"

	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// be-sdk-go 的 gRPC 端口不验 JWT，ctx 里没有调用者的 Claims。面向用户的 rpc 都要
// 调用者的数据范围（建单要拍部门快照，单张订单的命令与读要判范围，列表按范围过滤），
// 没有身份时 besdk.ScopeOf 会 panic，被 recovery 接成一个 Internal 加一行 ERROR。
// 这是调用方的错，答 Unauthenticated，在碰到任何东西之前；组件之间的三个 rpc
// （CalculatePriceDryRun、BatchGetOrder、GetOrderStatus）不带范围，照常工作。
func TestGRPC_没有调用者身份_面向用户的rpc答Unauthenticated(t *testing.T) {
	h := newSalesHarness(t)
	order := h.createDraft("noauth")
	ctx := metadata.AppendToOutgoingContext(context.Background(), noClaimsKey, "1")

	calls := map[string]func() error{
		"CreateOrder": func() error {
			_, err := h.sales.CreateOrder(ctx, &salesv1.CreateOrderRequest{IdempotencyKey: h.key("noauth-create2"),
				CustomerId: "C-harness-" + h.sfx, Items: []*salesv1.CreateOrderItemInput{{ProductId: "P-harness", Qty: "1"}}})
			return err
		},
		"ConfirmOrder": func() error {
			_, err := h.sales.ConfirmOrder(ctx, &salesv1.ConfirmOrderRequest{IdempotencyKey: h.key("noauth-confirm"), OrderId: order.Id})
			return err
		},
		"CancelOrder": func() error {
			_, err := h.sales.CancelOrder(ctx, &salesv1.CancelOrderRequest{IdempotencyKey: h.key("noauth-cancel"), OrderId: order.Id})
			return err
		},
		"ShipOrder": func() error {
			_, err := h.sales.ShipOrder(ctx, &salesv1.ShipOrderRequest{IdempotencyKey: h.key("noauth-ship"), OrderId: order.Id})
			return err
		},
		"GetOrder": func() error {
			_, err := h.sales.GetOrder(ctx, &salesv1.GetOrderRequest{Id: order.Id})
			return err
		},
		"ListOrders": func() error {
			_, err := h.sales.ListOrders(ctx, &salesv1.ListOrdersRequest{})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s 没有调用者身份：期望 Unauthenticated，实际 %v", name, err)
		}
	}
	if n := h.deps.reserves.Load() + h.deps.cancels.Load() + h.deps.issues.Load(); n != 0 {
		t.Errorf("没有身份的调用不该碰到 erp/inventory，实际调用了 %d 次", n)
	}

	if _, err := h.sales.CalculatePriceDryRun(ctx, &salesv1.CalculatePriceDryRunRequest{
		Items: []*salesv1.PriceDryRunItem{{ProductId: "P-harness", Qty: "1"}}}); err != nil {
		t.Errorf("CalculatePriceDryRun 是组件间协议，不要调用者身份，实际 %v", err)
	}
	if _, err := h.sales.BatchGetOrder(ctx, &salesv1.BatchGetOrderRequest{Ids: []string{order.Id}}); err != nil {
		t.Errorf("BatchGetOrder 是组件间协议，不要调用者身份，实际 %v", err)
	}
	if _, err := h.sales.GetOrderStatus(ctx, &salesv1.GetOrderStatusRequest{OrderId: order.Id}); err != nil {
		t.Errorf("GetOrderStatus 是组件间协议，不要调用者身份，实际 %v", err)
	}
}
