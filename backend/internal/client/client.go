// Package client 拨号到本组件依赖的 gRPC 服务，返回类型化 stub。
//
// ⚠️ 不带 System 后缀的一组（Customer/Product/Inventory/Workflow）
// 走 besdk.UserClient，透传调用者的 JWT，下游按调用者身份做数据范围过滤；
// 只许出现在用户请求路径上。*System 一组走 besdk.SystemClient，带本组件
// 自己的身份、绕过下游数据范围，只许出现在事件 handler 里（目前只有
// crm.opportunity.won.v1 的消费路径，那里没有 JWT 可透传）。
//
// 地址一律从 Clients 持有的 besdk.Config 读（rt.Config），dep 用组件 ID，
// extra 固定是 "grpc"（各依赖 component.yaml 的 extraPorts {name: grpc}）。
//
// 每个方法返回 (客户端, 关闭函数, error)，调用方 defer 关闭函数即可。
// 没有连接池：每次调用现拨一条（同 besdk.UserClient 的约定）。
package client

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	besdk "github.com/brickKit/be-sdk-go"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
)

// workflowDep 是唯一的可选依赖：缺席时平台不注入它的地址变量（不是空串）。
const workflowDep = "infra/workflow"

// Clients 持有拨号要用的配置（模块的 rt.Config）。
type Clients struct {
	cfg besdk.Config
}

// New 用模块的 rt.Config 构造。
func New(cfg besdk.Config) *Clients { return &Clients{cfg: cfg} }

// WorkflowConfigured 报告可选依赖 infra/workflow 是否装配（地址变量存在
// 且非空）。调用 Workflow/WorkflowSystem 之前先判它：拨号本身在缺席时也会
// 返回 error，但那个错误分不清"没装这个组件"与"装了但连不上"。
func (c *Clients) WorkflowConfigured() bool {
	_, ok := c.cfg.Endpoint(workflowDep, "grpc")
	return ok
}

func (c *Clients) dial(ctx context.Context, dep string) (*grpc.ClientConn, error) {
	conn, err := besdk.UserClient(ctx, c.cfg, dep, "grpc")
	if err != nil {
		return nil, fmt.Errorf("拨号 %s: %w", dep, err)
	}
	return conn, nil
}

// dialSystem 只许在事件 handler 里调用：那里没有 JWT 可透传。被调用的
// rpc（BatchGet/Reserve/CancelReservation/CreateTask）本身是不做数据范围
// 过滤的组件间协议，所以系统身份在这里没有绕过任何过滤。
func (c *Clients) dialSystem(dep string) (*grpc.ClientConn, error) {
	conn, err := besdk.SystemClient(c.cfg, dep, "grpc")
	if err != nil {
		return nil, fmt.Errorf("拨号（系统身份）%s: %w", dep, err)
	}
	return conn, nil
}

func noop() error { return nil }

// Customer 拨一条到 mdm/customer 的连接（透传调用者身份）。
func (c *Clients) Customer(ctx context.Context) (customerv1.CustomerServiceClient, func() error, error) {
	conn, err := c.dial(ctx, "mdm/customer")
	if err != nil {
		return nil, noop, err
	}
	return customerv1.NewCustomerServiceClient(conn), conn.Close, nil
}

// Product 拨一条到 mdm/product 的连接（透传调用者身份）。
func (c *Clients) Product(ctx context.Context) (productv1.ProductServiceClient, func() error, error) {
	conn, err := c.dial(ctx, "mdm/product")
	if err != nil {
		return nil, noop, err
	}
	return productv1.NewProductServiceClient(conn), conn.Close, nil
}

// Inventory 拨一条到 erp/inventory 的连接（透传调用者身份）：Reserve /
// CancelReservation / ConfirmIssue / GetReservationStatus 都走它。
func (c *Clients) Inventory(ctx context.Context) (inventoryv1.InventoryServiceClient, func() error, error) {
	conn, err := c.dial(ctx, "erp/inventory")
	if err != nil {
		return nil, noop, err
	}
	return inventoryv1.NewInventoryServiceClient(conn), conn.Close, nil
}

// Workflow 拨一条到 infra/workflow 的连接（透传调用者身份）。可选依赖：
// 先判 WorkflowConfigured。
func (c *Clients) Workflow(ctx context.Context) (workflowv1.WorkflowServiceClient, func() error, error) {
	conn, err := c.dial(ctx, workflowDep)
	if err != nil {
		return nil, noop, err
	}
	return workflowv1.NewWorkflowServiceClient(conn), conn.Close, nil
}

// ── 系统身份变体：只许在事件 handler 里调用（见 dialSystem 的注释）──

// CustomerSystem 同 Customer，系统身份。
func (c *Clients) CustomerSystem() (customerv1.CustomerServiceClient, func() error, error) {
	conn, err := c.dialSystem("mdm/customer")
	if err != nil {
		return nil, noop, err
	}
	return customerv1.NewCustomerServiceClient(conn), conn.Close, nil
}

// ProductSystem 同 Product，系统身份。
func (c *Clients) ProductSystem() (productv1.ProductServiceClient, func() error, error) {
	conn, err := c.dialSystem("mdm/product")
	if err != nil {
		return nil, noop, err
	}
	return productv1.NewProductServiceClient(conn), conn.Close, nil
}

// InventorySystem 同 Inventory，系统身份。
func (c *Clients) InventorySystem() (inventoryv1.InventoryServiceClient, func() error, error) {
	conn, err := c.dialSystem("erp/inventory")
	if err != nil {
		return nil, noop, err
	}
	return inventoryv1.NewInventoryServiceClient(conn), conn.Close, nil
}

// WorkflowSystem 同 Workflow，系统身份；同样先判 WorkflowConfigured。
func (c *Clients) WorkflowSystem() (workflowv1.WorkflowServiceClient, func() error, error) {
	conn, err := c.dialSystem(workflowDep)
	if err != nil {
		return nil, noop, err
	}
	return workflowv1.NewWorkflowServiceClient(conn), conn.Close, nil
}
