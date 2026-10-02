package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// 调用方传来的列表游标是外部输入：解不开是调用方的错（400），不是服务端
// 内部错误（500）。
func TestListOrders_非法cursor映射成InvalidArgument(t *testing.T) {
	svc, _ := newTestService(t)
	authed := besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: "u_manager", DeptPath: "/1/12/"})

	_, err := svc.ListOrders(authed, repo.ListInput{Cursor: "!!!不是游标"})
	if err == nil {
		t.Fatal("非法 cursor 应该报错")
	}
	if got := status.Code(ToStatus(err)); got != codes.InvalidArgument {
		t.Fatalf("非法 cursor 应映射成 InvalidArgument，实际 %v（%v）", got, err)
	}
}

// BatchGetOrder 的约定：找到的放 orders，找不到的 id 放 missing_ids，不报错
// ——调用方用它防 N+1，一批里有一个 id 失效不该让整批失败。
func TestBatchGetOrder_找不到的id进missing不报错(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()
	order, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniqueSuffix("svc-batchget"), CustomerID: "C-1", CustomerName: "测试客户",
		Items: testItems(), DeptPath: "/1/12/", OwnerID: "u_owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	const absent = "987654321987"
	found, missing, err := svc.BatchGetOrder(ctx, []string{order.ID, absent})
	if err != nil {
		t.Fatalf("有一个 id 不存在时不该整批报错，实际：%v", err)
	}
	if len(found) != 1 || found[0].ID != order.ID {
		t.Fatalf("期望找到 1 张订单 %s，实际 %+v", order.ID, found)
	}
	if len(missing) != 1 || missing[0] != absent {
		t.Fatalf("期望 missing=[%s]，实际 %v", absent, missing)
	}
}
