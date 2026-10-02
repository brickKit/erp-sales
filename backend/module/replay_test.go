package module

import (
	"context"
	"testing"

	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 契约（sales.proto）：每个写命令都带 idempotency_key，"同一个键重放返回第一次
// 的结果"。重放正是响应丢了的那个调用方（客户端超时、代理断开）：第一次已经
// 成功，第二次必须拿到同一张单，而不是"状态不对"——也不能再调一次依赖。

func TestReplay_确认订单同一个键重放返回第一次的结果_不再预留(t *testing.T) {
	h := newSalesHarness(t)
	ctx := context.Background()
	order := h.createDraft("confirm")
	req := &salesv1.ConfirmOrderRequest{IdempotencyKey: h.key("confirm"), OrderId: order.Id}

	first, err := h.sales.ConfirmOrder(ctx, req)
	if err != nil {
		t.Fatalf("第一次确认失败：%v", err)
	}
	second, err := h.sales.ConfirmOrder(ctx, req)
	if err != nil {
		t.Fatalf("同一个键重放确认应返回第一次的结果，实际：%v", err)
	}
	if second.Order.Id != first.Order.Id || second.Order.Status != salesv1.OrderStatus_ORDER_STATUS_CONFIRMED ||
		second.Order.ReservationId != first.Order.ReservationId {
		t.Fatalf("重放结果与第一次不同：第一次 %+v，重放 %+v", first.Order, second.Order)
	}
	if n := h.deps.reserves.Load(); n != 1 {
		t.Fatalf("重放不该再向 erp/inventory 预留，Reserve 调用了 %d 次", n)
	}
}

func TestReplay_发货同一个键重放返回第一次的结果_不再出库(t *testing.T) {
	h := newSalesHarness(t)
	ctx := context.Background()
	order := h.createDraft("ship")
	if _, err := h.sales.ConfirmOrder(ctx, &salesv1.ConfirmOrderRequest{IdempotencyKey: h.key("ship-confirm"), OrderId: order.Id}); err != nil {
		t.Fatalf("确认失败：%v", err)
	}
	req := &salesv1.ShipOrderRequest{IdempotencyKey: h.key("ship"), OrderId: order.Id}

	if _, err := h.sales.ShipOrder(ctx, req); err != nil {
		t.Fatalf("第一次发货失败：%v", err)
	}
	second, err := h.sales.ShipOrder(ctx, req)
	if err != nil {
		t.Fatalf("同一个键重放发货应返回第一次的结果，实际：%v", err)
	}
	if second.Order.Id != order.Id || second.Order.Status != salesv1.OrderStatus_ORDER_STATUS_SHIPPED {
		t.Fatalf("重放结果不对：%+v", second.Order)
	}
	if n := h.deps.issues.Load(); n != 1 {
		t.Fatalf("重放不该再向 erp/inventory 确认出库，ConfirmIssue 调用了 %d 次", n)
	}
}

func TestReplay_取消已确认的订单同一个键重放返回第一次的结果_不再释放(t *testing.T) {
	h := newSalesHarness(t)
	ctx := context.Background()
	order := h.createDraft("cancel")
	if _, err := h.sales.ConfirmOrder(ctx, &salesv1.ConfirmOrderRequest{IdempotencyKey: h.key("cancel-confirm"), OrderId: order.Id}); err != nil {
		t.Fatalf("确认失败：%v", err)
	}
	req := &salesv1.CancelOrderRequest{IdempotencyKey: h.key("cancel"), OrderId: order.Id, Reason: "重放测试"}

	if _, err := h.sales.CancelOrder(ctx, req); err != nil {
		t.Fatalf("第一次取消失败：%v", err)
	}
	second, err := h.sales.CancelOrder(ctx, req)
	if err != nil {
		t.Fatalf("同一个键重放取消应返回第一次的结果，实际：%v", err)
	}
	if second.Order.Status != salesv1.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("重放结果不对：%+v", second.Order)
	}
	if n := h.deps.cancels.Load(); n != 1 {
		t.Fatalf("重放不该再向 erp/inventory 释放预留，CancelReservation 调用了 %d 次", n)
	}
}

// 建单的重放同样只看幂等键：第一次之后客户被停用了，重放也要拿回第一次建出的
// 那张单，而不是重新校验一遍客户再失败。
func TestReplay_建单同一个键重放返回第一次的结果_不再校验依赖(t *testing.T) {
	h := newSalesHarness(t)
	ctx := context.Background()
	first := h.createDraft("create")
	calls := h.deps.customerCalls.Load()
	h.deps.customerInactive.Store(true)

	second, err := h.sales.CreateOrder(ctx, &salesv1.CreateOrderRequest{
		IdempotencyKey: h.key("create-create"), CustomerId: "C-harness-" + h.sfx,
		Items: []*salesv1.CreateOrderItemInput{{ProductId: "P-harness", Qty: "1"}},
	})
	if err != nil {
		t.Fatalf("同一个键重放建单应返回第一次的结果，实际：%v", err)
	}
	if second.Order.Id != first.Id {
		t.Fatalf("重放建单应返回第一次的订单 %s，实际 %s", first.Id, second.Order.Id)
	}
	if n := h.deps.customerCalls.Load(); n != calls {
		t.Fatalf("重放不该再调 mdm/customer，多调了 %d 次", n-calls)
	}
}

// 一个幂等键只属于一次命令、一张订单（同 infra/workflow 的规则）：拿确认用过的
// 键去发货，照样短路会把确认的结果当成"已经发过货"返回——更糟的是在那之前已经
// 向 erp/inventory 出了库。拿 A 单用过的键去确认 B 单，会把 A 单当成 B 单的结果。
func TestReplay_键被别的命令或别的订单用过_答InvalidArgument且不调依赖(t *testing.T) {
	h := newSalesHarness(t)
	ctx := context.Background()
	a := h.createDraft("bind-a")
	b := h.createDraft("bind-b")
	confirmKey := h.key("bind-confirm")
	if _, err := h.sales.ConfirmOrder(ctx, &salesv1.ConfirmOrderRequest{IdempotencyKey: confirmKey, OrderId: a.Id}); err != nil {
		t.Fatalf("确认 A 失败：%v", err)
	}

	_, err := h.sales.ShipOrder(ctx, &salesv1.ShipOrderRequest{IdempotencyKey: confirmKey, OrderId: a.Id})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("确认用过的键拿去发货：期望 InvalidArgument，实际 %v", err)
	}
	if n := h.deps.issues.Load(); n != 0 {
		t.Fatalf("键冲突时不该向 erp/inventory 出库，ConfirmIssue 调用了 %d 次", n)
	}

	_, err = h.sales.ConfirmOrder(ctx, &salesv1.ConfirmOrderRequest{IdempotencyKey: confirmKey, OrderId: b.Id})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("A 单用过的键拿去确认 B 单：期望 InvalidArgument，实际 %v", err)
	}
	if n := h.deps.reserves.Load(); n != 1 {
		t.Fatalf("键冲突时不该再预留，Reserve 一共调用了 %d 次（期望只有 A 的 1 次）", n)
	}
	got, err := h.sales.GetOrder(ctx, &salesv1.GetOrderRequest{Id: b.Id})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != salesv1.OrderStatus_ORDER_STATUS_DRAFT {
		t.Fatalf("B 单不该被动过，实际 %v", got.Status)
	}

	_, err = h.sales.CancelOrder(ctx, &salesv1.CancelOrderRequest{IdempotencyKey: confirmKey, OrderId: a.Id})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("确认用过的键拿去取消：期望 InvalidArgument，实际 %v", err)
	}
	if n := h.deps.cancels.Load(); n != 0 {
		t.Fatalf("键冲突时不该释放预留，CancelReservation 调用了 %d 次", n)
	}
}
