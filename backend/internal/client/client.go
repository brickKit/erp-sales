// Package client 拨号到本组件四条强依赖边的 gRPC 客户端 stub——本阶段
// 第一次真实的跨组件调用（设计计划 §9：契约只读镜像 + 本地生成客户端
// stub，contracts/vendor/ 的四份镜像 buf generate 出来的 gen/ 包在这里
// 被组装成可用的 XxxServiceClient）。
//
// ⚠️ 全部走 besdk.UserClient（透传 JWT），不许换成 SystemClient——这是
// 用户请求路径，用 SystemClient 会绕过下游数据权限（设计计划 §3.1、
// 导读第 21 条）。地址走 besdk.Endpoint() 剥 http://，dep 参数用组件 ID、
// extra 参数固定是 "grpc"（对应各组件 component.yaml 的 extraPorts
// {name: grpc}，见 registry/ports.tsv）。
//
// 每个函数返回 (客户端, 关闭函数, error)——调用方 defer 关闭函数即可，
// 不需要关心连接细节。阶段二没有连接池/复用（同 besdk.UserClient 自己
// 的既有约定：每次用户请求现拨一条），这里的四个函数只是把"拨号 + 包一层
// 具体类型的 stub"这件事各写一遍。
package client

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	besdk "github.com/brickKit/be-sdk-go"

	financev1 "github.com/brickKit/erp-sales/gen/erp/finance/v1"
	inventoryv1 "github.com/brickKit/erp-sales/gen/erp/inventory/v1"
	workflowv1 "github.com/brickKit/erp-sales/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/erp-sales/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/erp-sales/gen/mdm/product/v1"
)

// dial 是四个 New* 函数共用的拨号逻辑，dep 是 component.yaml 里的组件 ID。
func dial(ctx context.Context, dep string) (*grpc.ClientConn, error) {
	conn, err := besdk.UserClient(ctx, dep, "grpc")
	if err != nil {
		return nil, fmt.Errorf("拨号 %s: %w", dep, err)
	}
	return conn, nil
}

// Customer 拨一条到 mdm-customer 的连接，返回类型化 stub。
func Customer(ctx context.Context) (customerv1.CustomerServiceClient, func() error, error) {
	conn, err := dial(ctx, "mdm/customer")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return customerv1.NewCustomerServiceClient(conn), conn.Close, nil
}

// Product 拨一条到 mdm-product 的连接，返回类型化 stub。
func Product(ctx context.Context) (productv1.ProductServiceClient, func() error, error) {
	conn, err := dial(ctx, "mdm/product")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return productv1.NewProductServiceClient(conn), conn.Close, nil
}

// Inventory 拨一条到 erp-inventory 的连接，返回类型化 stub——TCC 四件套
// (Reserve/CancelReservation/ConfirmIssue/GetReservationStatus) 都走它。
func Inventory(ctx context.Context) (inventoryv1.InventoryServiceClient, func() error, error) {
	conn, err := dial(ctx, "erp/inventory")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return inventoryv1.NewInventoryServiceClient(conn), conn.Close, nil
}

// Finance 拨一条到 erp-finance 的连接，返回类型化 stub——供改历史单/
// 定时对账用（CheckPeriodOpen/BatchGetCreditExposure），不在 ConfirmOrder
// 的实时 TCC 链里（设计计划 §3.1 的链只有①-⑤，erp-finance 不在其中）。
func Finance(ctx context.Context) (financev1.FinanceServiceClient, func() error, error) {
	conn, err := dial(ctx, "erp/finance")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return financev1.NewFinanceServiceClient(conn), conn.Close, nil
}

// Workflow 拨一条到 infra-workflow 的连接，返回类型化 stub——⚠️ 唯一的
// 弱依赖：调用方必须先用 besdk.Endpoint("infra/workflow", "grpc") 的
// 二值返回判断这条依赖有没有被装配（设计计划 §5、§9 待决问题 §4.4.4），
// ok == false 就跳过整个建异常待办的动作，不要走到这个函数——dial()
// 内部的 UserClient 虽然在端点缺失时也会返回一个干净的 error（不会
// panic），但那个错误信息不区分"没装这个组件"与"装了但连不上"，调用方
// 想把两种情况分开记日志就必须自己先判一次 Endpoint。
func Workflow(ctx context.Context) (workflowv1.WorkflowServiceClient, func() error, error) {
	conn, err := dial(ctx, "infra/workflow")
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return workflowv1.NewWorkflowServiceClient(conn), conn.Close, nil
}
