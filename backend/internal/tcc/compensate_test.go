package tcc

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
)

// 这里测的是补偿的计数与挂起规则本身（02-backend.md：补偿失败超过三次
// 转 SUSPENDED 并开异常待办）。CancelReservation 由一个按脚本失败的替身
// 回答；释放库存是否真的生效由 confirm_test.go 里连真实 erp-inventory
// 的测试负责。

// scriptedInventory 前 failFirst 次 CancelReservation 返回错误，之后成功；
// ctx 已经取消时照实返回 ctx 的错误（真实 gRPC 客户端就是这样）。
type scriptedInventory struct {
	inventoryv1.InventoryServiceClient
	failFirst int
	calls     int
	succeeded int
	keys      []string
}

func (f *scriptedInventory) CancelReservation(ctx context.Context, in *inventoryv1.CancelReservationRequest, _ ...grpc.CallOption) (*inventoryv1.CancelReservationResponse, error) {
	f.calls++
	f.keys = append(f.keys, in.IdempotencyKey)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.calls <= f.failFirst {
		return nil, errors.New("模拟 CancelReservation 失败")
	}
	f.succeeded++
	return &inventoryv1.CancelReservationResponse{}, nil
}

func newCompensationFixture(t *testing.T) (*Orchestrator, *repo.Repo, *repo.Order) {
	t.Helper()
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	order, err := r.CreateOrder(context.Background(), repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("comp-rule-order"), CustomerID: "C-comp-rule", CustomerName: "补偿规则测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-comp-rule", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "1", Discount: "0", TaxRate: "0", Subtotal: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// infra/workflow 未装配：挂起时只打日志，不建待办（建待办由 confirm_test.go 的真实测试覆盖）。
	orch := New(r, client.New(besdk.NewConfig(nil)), testWarehouseID, time.Second, "u_ops")
	return orch, r, order
}

func reload(t *testing.T, r *repo.Repo, id string) *repo.Order {
	t.Helper()
	o, err := r.GetOrder(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCompensateReserve_一直失败时重试三次后挂起(t *testing.T) {
	orch, r, order := newCompensationFixture(t)
	inv := &scriptedInventory{failFirst: 1000}

	orch.compensateReserve(context.Background(), inv, order.ID, uniqueSuffix("comp-always-fail"), "R-1", slog.Default())

	if inv.calls != 3 {
		t.Fatalf("补偿失败要原地重试，三次之后停：期望调用 3 次 CancelReservation，实际 %d 次", inv.calls)
	}
	got := reload(t, r, order.ID)
	if got.Status != repo.StatusSuspended || got.CompensationAttempts != 3 {
		t.Fatalf("三次都失败应转 SUSPENDED 且计数 3，实际 status=%s attempts=%d", got.Status, got.CompensationAttempts)
	}
	for _, k := range inv.keys {
		if k != inv.keys[0] {
			t.Fatalf("重试必须用同一个幂等键，实际 %v", inv.keys)
		}
	}
}

func TestCompensateReserve_重试中途成功不挂起且计数清零(t *testing.T) {
	orch, r, order := newCompensationFixture(t)
	inv := &scriptedInventory{failFirst: 2}

	orch.compensateReserve(context.Background(), inv, order.ID, uniqueSuffix("comp-third-ok"), "R-2", slog.Default())

	if inv.calls != 3 {
		t.Fatalf("前两次失败、第三次成功：期望调用 3 次，实际 %d 次", inv.calls)
	}
	got := reload(t, r, order.ID)
	if got.Status != repo.StatusDraft || got.CompensationAttempts != 0 {
		t.Fatalf("补偿最终成功：订单应仍是 DRAFT、连续失败计数清零，实际 status=%s attempts=%d", got.Status, got.CompensationAttempts)
	}
}

func TestCompensateReserve_一次成功不计失败(t *testing.T) {
	orch, r, order := newCompensationFixture(t)
	inv := &scriptedInventory{}

	orch.compensateReserve(context.Background(), inv, order.ID, uniqueSuffix("comp-ok"), "R-3", slog.Default())

	if inv.calls != 1 {
		t.Fatalf("一次成功就不再重试：期望调用 1 次，实际 %d 次", inv.calls)
	}
	got := reload(t, r, order.ID)
	if got.Status != repo.StatusDraft || got.CompensationAttempts != 0 {
		t.Fatalf("补偿成功：期望 DRAFT 且计数 0，实际 status=%s attempts=%d", got.Status, got.CompensationAttempts)
	}
}

// 用户请求断开（ctx 取消）时，已经成功的预留仍必须被释放：补偿不能跟着
// 请求一起放弃，否则库存被白白占住。
func TestCompensateReserve_请求取消后仍完成补偿(t *testing.T) {
	orch, r, order := newCompensationFixture(t)
	inv := &scriptedInventory{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch.compensateReserve(ctx, inv, order.ID, uniqueSuffix("comp-cancelled"), "R-4", slog.Default())

	got := reload(t, r, order.ID)
	if inv.succeeded != 1 || got.Status != repo.StatusDraft || got.CompensationAttempts != 0 {
		t.Fatalf("请求已取消也要完成补偿：期望释放成功 1 次、DRAFT、计数 0，实际 succeeded=%d calls=%d status=%s attempts=%d",
			inv.succeeded, inv.calls, got.Status, got.CompensationAttempts)
	}
}
