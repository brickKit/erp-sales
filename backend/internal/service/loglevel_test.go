package service

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/tcc"
)

// 命令失败时的日志级别：ERROR 只留给运维必须处理的事（依赖不可达、数据库
// 出错、内部错误）。调用方自己能看懂、会收到 4xx 的失败（订单状态不对、
// 参数不合法、看不到这张单）不记 ERROR——否则每一次正常的业务拒绝都在
// 告警里刷屏，真正的故障反而被淹没。

type logRecord struct {
	level slog.Level
	msg   string
}

type captureHandler struct {
	mu   *sync.Mutex
	recs *[]logRecord
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, logRecord{r.Level, r.Message})
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

func newLoggedService(t *testing.T) (*Service, *repo.Repo, *sql.DB, func() []logRecord) {
	t.Helper()
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	var mu sync.Mutex
	var recs []logRecord
	logger := slog.New(captureHandler{&mu, &recs})
	// 没有任何依赖地址：凡是要拨号的命令都会以"拨号失败"告终
	orch := tcc.New(r, client.New(besdk.NewConfig(nil)), "1", time.Second, "")
	return New(r, orch, logger), r, db, func() []logRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]logRecord(nil), recs...)
	}
}

func levelOf(t *testing.T, recs []logRecord, msg string) slog.Level {
	t.Helper()
	for _, r := range recs {
		if r.msg == msg {
			return r.level
		}
	}
	t.Fatalf("没有记下 %q 这条日志，实际记录：%+v", msg, recs)
	return 0
}

func TestCommandLogLevel_调用方的错误不记ERROR(t *testing.T) {
	svc, r, db, logs := newLoggedService(t)
	ctx := besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: "u_loglevel", DeptPath: "/loglevel/"})
	newOrder := func() *repo.Order {
		o, err := r.CreateOrder(ctx, repo.CreateOrderInput{
			IdempotencyKey: uniqueSuffix("loglevel-create"), CustomerID: "C-loglevel", CustomerName: "日志级别测试客户",
			Items: testItems(), DeptPath: "/loglevel/", OwnerID: "u_loglevel",
		})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	// 草稿不能发货（FailedPrecondition → 400）
	draft := newOrder()
	if _, err := svc.ShipOrder(ctx, draft.ID, uniqueSuffix("loglevel-ship")); err == nil {
		t.Fatal("草稿发货应失败")
	}
	// 已取消的单不能确认（FailedPrecondition → 400）
	cancelled := newOrder()
	if _, err := svc.CancelOrder(ctx, cancelled.ID, uniqueSuffix("loglevel-cancel"), "测试"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmOrder(ctx, cancelled.ID, uniqueSuffix("loglevel-confirm")); err == nil {
		t.Fatal("已取消的单确认应失败")
	}
	// 挂起的单不能取消（FailedPrecondition → 400）
	suspended := newOrder()
	if err := besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		return repo.SuspendOrderTx(ctx, tx, mustID(t, suspended.ID), "测试挂起")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelOrder(ctx, suspended.ID, uniqueSuffix("loglevel-cancel-suspended"), "测试"); err == nil {
		t.Fatal("挂起的单取消应失败")
	}

	recs := logs()
	for _, msg := range []string{"发货失败", "确认订单失败", "取消订单失败"} {
		if lv := levelOf(t, recs, msg); lv >= slog.LevelError {
			t.Errorf("%q 是调用方的错误（4xx），不该记 ERROR，实际 %s", msg, lv)
		}
	}
}

func TestCommandLogLevel_依赖不可达仍记ERROR(t *testing.T) {
	svc, _, _, logs := newLoggedService(t)
	ctx := besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: "u_loglevel", DeptPath: "/loglevel/"})
	if _, err := svc.CreateOrder(ctx, uniqueSuffix("loglevel-dial"), "C-loglevel",
		[]CreateOrderItem{{ProductID: "P-loglevel", Qty: "1"}}); err == nil {
		t.Fatal("没有 mdm/customer 地址时建单应失败")
	}
	if lv := levelOf(t, logs(), "建单失败"); lv != slog.LevelError {
		t.Fatalf("拨不通依赖是运维要处理的事，应记 ERROR，实际 %s", lv)
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
