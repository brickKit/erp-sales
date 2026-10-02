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
