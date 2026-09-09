// Package tcc 编排本组件唯一的跨组件写链路——ConfirmOrder 的 TCC 补偿链
// （设计计划 §3.1，本阶段最难的一段代码），以及 CancelOrder/ShipOrder 里
// 同样需要"先调 erp-inventory、再落本地状态"的部分。CreateOrder 不需要
// TCC（没有需要补偿的外部写），但同样需要先调 mdm-customer/mdm-product
// 校验+快照，所以也放在这个包里，和真正的 TCC 链共用"先网络调用、
// 后本地事务"的骨架。
//
// ⚠️ 这一层只做编排，不碰 SQL——本地读写全部委托给 repo 包
// （backend/internal/repo/write.go 的 Finalize* 系列），网络调用全部走
// backend/internal/client 拨出去的 UserClient。
package tcc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-sales/backend/internal/client"
	"github.com/brickKit/erp-sales/backend/internal/repo"

	inventoryv1 "github.com/brickKit/erp-sales/gen/erp/inventory/v1"
	customerv1 "github.com/brickKit/erp-sales/gen/mdm/customer/v1"
	productv1 "github.com/brickKit/erp-sales/gen/mdm/product/v1"
)

// errReservePending 是"预留状态未知/未提交，稍后重试或等对账兜底"的
// 哨兵——不是系统内部错误，service/grpc 两层通过 ToStatus 把它映射成
// FailedPrecondition/Unavailable（设计计划 §4.5）。
var errReservePending = errors.New("库存预留状态未确定")

// ErrReservePending 导出给 service 层做 errors.Is 判断。
var ErrReservePending = errReservePending

// Orchestrator 持有编排四条命令需要的一切：本地仓储 + 平台级配置。
type Orchestrator struct {
	Repo               *repo.Repo
	DefaultWarehouseID string        // 设计计划 §9 第 9 条：阶段二没有多仓选货逻辑
	ReserveTimeout     time.Duration // Reserve/GetReservationStatus 各自的超时（§4.5 的"掐短"）
	// ExceptionAssigneeSub 是补偿连续失败时建的 exception 待办分配给谁——
	// 本阶段没有 mdm-org，用最简单的配置项形式过（阶段三 Task 8 明文
	// 要求，不要提前实现组织树路由，设计计划 §4.4.4）。留空表示这项
	// 还没配置，跳过建待办只打日志（同 infra/workflow 弱依赖缺失的
	// 判据——两个独立的"跳过"开关，见 confirm.go 的 maybeCreateExceptionTask）。
	ExceptionAssigneeSub string
}

func New(r *repo.Repo, defaultWarehouseID string, reserveTimeout time.Duration, exceptionAssigneeSub string) *Orchestrator {
	return &Orchestrator{
		Repo: r, DefaultWarehouseID: defaultWarehouseID, ReserveTimeout: reserveTimeout,
		ExceptionAssigneeSub: exceptionAssigneeSub,
	}
}

// isDeadlineExceeded 判断一次 gRPC 调用是不是因为 ctx 超时/取消而失败——
// 与"服务端明确拒绝"（如库存不足的 FailedPrecondition）必须分开处理，
// 后者严禁走"先 GetReservationStatus 再决定"这条路径，因为服务端已经
// 明确告诉我们请求根本没有被受理成功（设计计划 §4.5：只有响应没收到、
// 不代表请求没处理的情形才需要状态内省）。
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

func fetchActiveCustomer(ctx context.Context, customerID string) (*customerv1.Customer, error) {
	conn, closeConn, err := client.Customer(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	resp, err := conn.BatchGet(ctx, &customerv1.BatchGetRequest{Ids: []string{customerID}})
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

func fetchActiveProducts(ctx context.Context, productIDs []string) (map[string]*productv1.Product, error) {
	conn, closeConn, err := client.Product(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	resp, err := conn.BatchGet(ctx, &productv1.BatchGetRequest{Ids: productIDs})
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
// 组件级配置的默认仓库（设计计划 §9 第 9 条：阶段二没有多仓选货逻辑，
// erp-inventory 契约也还没有按 code 查仓库 id 的 rpc）。
func (o *Orchestrator) buildReserveItems(items []repo.OrderItem) []*inventoryv1.ReserveItem {
	out := make([]*inventoryv1.ReserveItem, 0, len(items))
	for _, it := range items {
		out = append(out, &inventoryv1.ReserveItem{
			ProductId: it.ProductID, WarehouseId: o.DefaultWarehouseID, Qty: it.Qty,
		})
	}
	return out
}
