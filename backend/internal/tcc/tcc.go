// Package tcc 编排本组件唯一的跨组件写链路——ConfirmOrder 的 TCC 补偿链
// （本组件最难的一段代码），以及 CancelOrder / ShipOrder 里同样需要"先调
// erp-inventory、再落本地状态"的部分。CreateOrder 不需要 TCC（没有需要补偿
// 的外部写），但同样要先调 mdm-customer / mdm-product 校验并取快照，所以也
// 放在这里，和 TCC 链共用"先网络调用、后本地事务"的骨架。商机赢单自动转单
// （opportunity_won.go）是同一套编排的事件入口。
//
// 这一层只做编排，不碰 SQL——本地读写全部委托给 repo 包
// （backend/internal/repo/write.go 的 Finalize* 系列）。网络调用走
// backend/internal/client：用户请求路径上透传调用者身份（UserClient），
// 只有事件 handler 里用系统身份（SystemClient）。
package tcc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-inventory/gen/erp/inventory/v1"
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
)

// errReservePending 是"预留状态未知/未提交，稍后重试或等对账兜底"的
// 哨兵——不是系统内部错误，service/grpc 两层通过 ToStatus 把它映射成
// Unavailable（503，调用方带同一个幂等键稍后重试）。
var errReservePending = errors.New("库存预留状态未确定")

// ErrReservePending 导出给 service 层做 errors.Is 判断。
var ErrReservePending = errReservePending

// Orchestrator 持有编排四条命令需要的一切：本地仓储 + 平台级配置。
type Orchestrator struct {
	Repo               *repo.Repo
	Clients            *client.Clients // 拨号依赖组件（地址来自 rt.Config）
	DefaultWarehouseID string          // 确认订单时向哪个仓库预留：没有多仓选货逻辑，统一用这一个
	CallTimeout        time.Duration   // 每一次同步调用依赖组件各自的超时
	// ExceptionAssigneeSub 是补偿连续失败时建的 exception 待办分配给谁——
	// 没有组织树可以按部门路由，用一个配置项指定。留空表示还没配置，跳过建
	// 待办只打日志（与 infra/workflow 缺席是两个独立的"跳过"开关，见
	// confirm.go 的 maybeCreateExceptionTask）。
	ExceptionAssigneeSub string
}

func New(r *repo.Repo, clients *client.Clients, defaultWarehouseID string, callTimeout time.Duration, exceptionAssigneeSub string) *Orchestrator {
	return &Orchestrator{
		Repo: r, Clients: clients, DefaultWarehouseID: defaultWarehouseID, CallTimeout: callTimeout,
		ExceptionAssigneeSub: exceptionAssigneeSub,
	}
}

// isDeadlineExceeded 判断一次 gRPC 调用是不是因为超时而失败——
// 与"服务端明确拒绝"（如库存不足的 FailedPrecondition）必须分开处理，
// 后者严禁走"先 GetReservationStatus 再决定"这条路径，因为服务端已经
// 明确告诉我们请求没有被受理成功（只有"响应没收到、不代表请求没处理"的
// 情形才需要状态内省）。
func isDeadlineExceeded(err error) bool {
	return status.Code(err) == codes.DeadlineExceeded
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// ── 客户/产品校验：CreateOrder 与 ConfirmOrder 共用 ──

func (o *Orchestrator) fetchActiveCustomer(ctx context.Context, customerID string) (*customerv1.Customer, error) {
	conn, closeConn, err := o.Clients.Customer(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	resp, err := conn.BatchGet(callCtx, &customerv1.BatchGetRequest{Ids: []string{customerID}})
	if err != nil {
		return nil, fmt.Errorf("校验客户失败: %w", err)
	}
	if len(resp.Customers) == 0 {
		return nil, fmt.Errorf("%w: 客户不存在：customer_id=%s", repo.ErrInvalidArgument, customerID)
	}
	c := resp.Customers[0]
	if c.Status != customerv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE {
		return nil, fmt.Errorf("%w: 客户不可用：customer_id=%s status=%s", repo.ErrInvalidArgument, customerID, c.Status)
	}
	return c, nil
}

func (o *Orchestrator) fetchActiveProducts(ctx context.Context, productIDs []string) (map[string]*productv1.Product, error) {
	conn, closeConn, err := o.Clients.Product(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	callCtx, cancel := context.WithTimeout(ctx, o.CallTimeout)
	defer cancel()
	resp, err := conn.BatchGet(callCtx, &productv1.BatchGetRequest{Ids: productIDs})
	if err != nil {
		return nil, fmt.Errorf("校验产品失败: %w", err)
	}
	if len(resp.MissingIds) > 0 {
		return nil, fmt.Errorf("%w: 产品不存在：%v", repo.ErrInvalidArgument, resp.MissingIds)
	}
	byID := make(map[string]*productv1.Product, len(resp.Products))
	for _, p := range resp.Products {
		if p.Status != productv1.ProductStatus_PRODUCT_STATUS_ACTIVE {
			return nil, fmt.Errorf("%w: 产品不可用：product_id=%s status=%s", repo.ErrInvalidArgument, p.Id, p.Status)
		}
		byID[p.Id] = p
	}
	return byID, nil
}

// buildReserveItems 把订单行转成 erp-inventory 的 ReserveItem——统一用
// 组件级配置的默认仓库（没有多仓选货逻辑，erp-inventory 的契约也没有按
// 仓库 code 查 id 的 rpc）。
func (o *Orchestrator) buildReserveItems(items []repo.OrderItem) []*inventoryv1.ReserveItem {
	out := make([]*inventoryv1.ReserveItem, 0, len(items))
	for _, it := range items {
		out = append(out, &inventoryv1.ReserveItem{
			ProductId: it.ProductID, WarehouseId: o.DefaultWarehouseID, Qty: it.Qty,
		})
	}
	return out
}
