// Package module 是 erp-sales 唯一的装配入口。独立运行（cmd/server 的
// besdk.RunStandalone）与进外壳走同一个 New：模块只交回零件（HTTP handler、
// gRPC 注册函数、后台循环），谁去 Listen、谁开连接池、谁初始化 OTel 与信号
// 处理，全归调用方——这样同一份代码进外壳之后不会与别的成员互相覆盖。
package module

import (
	"context"
	"fmt"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	"google.golang.org/grpc"

	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/consumer"
	grpcapi "github.com/brickKit/erp-sales/v2/backend/internal/grpc"
	httpapi "github.com/brickKit/erp-sales/v2/backend/internal/http"
	"github.com/brickKit/erp-sales/v2/backend/internal/partition"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/service"
	"github.com/brickKit/erp-sales/v2/backend/internal/tcc"
)

// callTimeout 是每一次同步调用依赖组件的超时（BatchGet、Reserve、
// GetReservationStatus、CancelReservation、ConfirmIssue、CreateTask）。
// 不做成配置项：依赖都在同一张网络里，几秒足够覆盖网络抖动，又不会让
// 用户等太久。
const callTimeout = 5 * time.Second

// New 构造 erp-sales 模块。签名是外壳与 RunStandalone 共同依赖的约定，
// 不改。
func New(ctx context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	cfg, err := readConfig(rt.Config)
	if err != nil {
		return nil, err
	}
	schema, role := cfg.schema, cfg.role

	r := repo.New(rt.DB, role, schema)
	orch := tcc.New(r, client.New(rt.Config), cfg.defaultWarehouseID, callTimeout, cfg.exceptionAssigneeSub)
	svc := service.New(r, orch, rt.Logger)

	// HTTP：engine 必须用 besdk.NewGinEngine，它已挂好 OTel / request-id /
	// error→status / PII 脱敏日志 / RED 指标 / /healthz / /metrics。
	eng := besdk.NewGinEngine(rt)
	httpapi.RegisterRoutes(eng, svc)

	return &besdk.Module{
		HTTPHandler: eng,

		// gRPC 由调用方在 extraPorts 的 grpc 端口上 Listen：组件之间的调用
		// 都走它，进了外壳也不改成函数直调。
		RegisterGRPC: func(gs *grpc.Server) {
			salesv1.RegisterSalesServiceServer(gs, grpcapi.New(svc))
		},

		// 后台循环：Outbox 推送 + 周分区维护（event_outbox/event_inbox）+
		// 月分区维护（sales_orders/sales_order_items）+ 消费事件。四个
		// 循环必须并发跑，不能顺序调用。
		Start: func(ctx context.Context) error {
			errCh := make(chan error, 4)
			go func() { errCh <- besdk.StartOutboxPump(ctx, rt.DB, schema, rt.NATS, rt.Logger) }()
			go func() { errCh <- partition.Start(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- partition.StartMonthly(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- consumer.Start(ctx, rt.DB, role, schema, rt.NATS, orch, rt.Logger) }()

			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				return err // 返回 error，不 log.Fatal：一个模块退出进程，同一外壳里的成员全跟着没了
			}
		},
		Stop: func(ctx context.Context) error { return nil }, // 后台循环靠 ctx 退出
	}, nil
}

// settings 是本模块自己的配置（configSchema 里的键）。
type settings struct {
	schema, role         string
	defaultWarehouseID   string
	exceptionAssigneeSub string
}

// readConfig 读本模块的配置，键名就是 configSchema 里的环境变量名。
//
// 配置只从 rt.Config 读，不碰 os.Getenv：一个进程只有一份环境，进外壳后各成员
// 的 PG_SCHEMA 会互相覆盖，不报错，模块就按别人的 schema 读写数据。
//
// DEFAULT_WAREHOUSE_ID 必填：没有多仓选货逻辑，确认订单时统一向这一个仓库发起
// 预留。缺了（或是空串）返回 error 让 New 失败，不 panic——进了外壳，一个成员
// panic 会让同一外壳里的成员全跟着退出。
//
// EXCEPTION_ASSIGNEE_SUB 可以留空（没有组织树可以按部门路由待办）：留空时补偿
// 连续失败只记日志、不开异常待办，补偿本身照常进行。
func readConfig(c besdk.Config) (settings, error) {
	schema := c.StringOr("PG_SCHEMA", "erp_sales")
	warehouse, _ := c.String("DEFAULT_WAREHOUSE_ID")
	if warehouse == "" {
		return settings{}, fmt.Errorf("必填配置 DEFAULT_WAREHOUSE_ID 没有值：确认订单时要向这个仓库预留库存")
	}
	return settings{
		// role 是每个事务里 SET LOCAL ROLE 的目标：独立运行时它就是登录角色；
		// 进外壳后外壳以自己的角色登录，靠这一步切到本组件的角色。
		schema: schema, role: schema + "_rw",
		defaultWarehouseID:   warehouse,
		exceptionAssigneeSub: c.StringOr("EXCEPTION_ASSIGNEE_SUB", ""),
	}, nil
}
