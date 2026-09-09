// Package module 是 erp-sales 唯一的装配入口（全局约束 §K、设计书
// §12.5.1、§13.3 铁律七）。单跑与合并走同一个 New 函数；模块只交回零件
// （handler、gRPC 注册函数、迁移、后台循环），谁去 Listen、谁开池、
// 谁 init OTel、谁装信号处理器，全归调用方。
package module

import (
	"context"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	salesv1 "github.com/brickKit/erp-sales/gen/erp/sales/v1"
	"google.golang.org/grpc"

	"github.com/brickKit/erp-sales/backend/internal/consumer"
	grpcapi "github.com/brickKit/erp-sales/backend/internal/grpc"
	httpapi "github.com/brickKit/erp-sales/backend/internal/http"
	"github.com/brickKit/erp-sales/backend/internal/partition"
	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/service"
	"github.com/brickKit/erp-sales/backend/internal/tcc"
	"github.com/brickKit/erp-sales/migrations"
)

// reserveTimeout 是 ConfirmOrder 的 TCC 链里 Reserve/GetReservationStatus
// 各自的超时（§4.5"掐短"判据）。阶段二没有把它做成配置项——四条依赖边
// 都在同一个 docker 网络里，几秒钟的超时足够覆盖真实的网络抖动而不会让
// 用户等太久；需要按部署环境调整时再回来补 configSchema。
const reserveTimeout = 5 * time.Second

// New 构造 erp-sales 模块。签名一个字都不许改（§12.5.1）。
func New(ctx context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	// ⚠️ 配置只从 rt.Config 来，模块里零 os.Getenv（§12.5.3、决策 110）。
	schema := rt.Config.StringOr("pgSchema", "erp_sales")
	role := schema + "_rw"
	// defaultWarehouseId 必填、无默认值（设计计划 §9 第 9 条）：阶段二
	// 没有多仓选货逻辑，ConfirmOrder 统一用这一个仓库发起 Reserve，配置
	// 缺失属于部署错误，不该带着空字符串跑起来（同 rt.Config.MustString
	// 自己的既有判据）。
	defaultWarehouseID := rt.Config.MustString("defaultWarehouseId")
	// exceptionAssigneeSub 留空是合法状态（本阶段没有 mdm-org，见
	// tcc.Orchestrator 的字段注释）——留空时 maybeCreateExceptionTask 只
	// 打日志跳过，不阻断 ConfirmOrder 本身的补偿逻辑。
	exceptionAssigneeSub := rt.Config.StringOr("exceptionAssigneeSub", "")

	r := repo.New(rt.DB, role, schema)
	orch := tcc.New(r, defaultWarehouseID, reserveTimeout, exceptionAssigneeSub)
	svc := service.New(r, orch, rt.Logger)

	// HTTP：engine 必须用 besdk.NewGinEngine，它已挂好 OTel / request-id /
	// error→status / PII 脱敏日志 / RED 指标 / /healthz / /metrics。
	eng := besdk.NewGinEngine(rt)
	httpapi.RegisterRoutes(eng, svc)

	return &besdk.Module{
		HTTPHandler: eng,

		// ⚠️ gRPC 一个不省，而且由调用方在 extraPorts["grpc"] 上 Listen
		// （§1.5 原则一）。
		RegisterGRPC: func(gs *grpc.Server) {
			salesv1.RegisterSalesServiceServer(gs, grpcapi.New(svc))
		},

		Migrations: migrations.FS, // 合并态由外壳按拓扑顺序跑（§13.3 铁律五）

		// 后台循环：Outbox 推送 + 周分区维护（event_outbox/event_inbox）+
		// 月分区维护（sales_orders/sales_order_items）+ 消费事件。四个
		// 循环必须并发跑，不能顺序调用。
		Start: func(ctx context.Context) error {
			errCh := make(chan error, 4)
			go func() { errCh <- besdk.StartOutboxPump(ctx, rt.DB, schema, rt.NATS, rt.Logger) }()
			go func() { errCh <- partition.Start(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- partition.StartMonthly(ctx, rt.DB, role, schema, rt.Logger) }()
			go func() { errCh <- consumer.Start(ctx, rt.DB, role, schema, rt.NATS, rt.Logger) }()

			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				return err // ⚠️ 返回 error，不许 log.Fatal：一个模块退进程 = 整组组件一起没了
			}
		},
		Stop: func(ctx context.Context) error { return nil }, // 后台循环靠 ctx 退出
	}, nil
}
