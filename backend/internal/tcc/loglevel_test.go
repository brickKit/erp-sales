package tcc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// 日志级别：ERROR 只留给运维必须处理的事。业务上的拒绝（参数、状态、库存
// 不足、额度不够）与调用方断开不是 ERROR；补偿的单次失败会原地重试，也不是；
// 连续失败到挂起订单才是。

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

func newCaptureLogger() (*slog.Logger, func() []logRecord) {
	var mu sync.Mutex
	var recs []logRecord
	return slog.New(captureHandler{&mu, &recs}), func() []logRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]logRecord(nil), recs...)
	}
}

func TestFailureLevel_按错误归属分级(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want slog.Level
	}{
		{"参数不合法", fmt.Errorf("%w: qty", repo.ErrInvalidArgument), slog.LevelInfo},
		{"订单不存在", fmt.Errorf("%w: id", repo.ErrNotFound), slog.LevelInfo},
		{"不是草稿", fmt.Errorf("%w: id", repo.ErrOrderNotDraft), slog.LevelInfo},
		{"终态", fmt.Errorf("%w: id", repo.ErrOrderTerminal), slog.LevelInfo},
		{"看不到", repo.ErrForbidden, slog.LevelInfo},
		{"上游拒绝：库存不足", fmt.Errorf("预留库存失败: %w", status.Error(codes.FailedPrecondition, "库存不足")), slog.LevelInfo},
		{"上游：找不到", status.Error(codes.NotFound, "x"), slog.LevelInfo},
		{"调用方断开", fmt.Errorf("查 sales_orders: %w", context.Canceled), slog.LevelInfo},
		{"上游 gRPC 取消", status.Error(codes.Canceled, "x"), slog.LevelInfo},
		{"预留状态未定", fmt.Errorf("%w: 已转入对账队列", ErrReservePending), slog.LevelWarn},
		{"上游超时", status.Error(codes.DeadlineExceeded, "x"), slog.LevelWarn},
		{"上游不可用", status.Error(codes.Unavailable, "x"), slog.LevelWarn},
		{"上游内部错误", status.Error(codes.Internal, "x"), slog.LevelError},
		{"拨号失败", errors.New("拨号 mdm/customer: 没有地址"), slog.LevelError},
	}
	for _, c := range cases {
		if got := FailureLevel(c.err); got != c.want {
			t.Errorf("%s：期望 %s，实际 %s", c.name, c.want, got)
		}
	}
}

func TestCompensateReserve_单次失败记WARN_挂起时记ERROR(t *testing.T) {
	orch, _, order := newCompensationFixture(t)
	logger, logs := newCaptureLogger()
	orch.compensateReserve(context.Background(), &scriptedInventory{failFirst: 1000}, order.ID, uniqueSuffix("comp-loglevel"), "R-1", logger)

	var attempts, errorsAtLevel int
	for _, r := range logs() {
		if r.msg == "补偿失败：CancelReservation 出错" {
			attempts++
			if r.level >= slog.LevelError {
				t.Errorf("单次补偿失败会原地重试，不该记 ERROR，实际 %s", r.level)
			}
		}
		if r.level >= slog.LevelError {
			errorsAtLevel++
		}
	}
	if attempts != 3 {
		t.Fatalf("期望记下 3 次补偿失败，实际 %d 次", attempts)
	}
	if errorsAtLevel != 1 {
		t.Fatalf("连续失败到挂起订单时应恰好记一条 ERROR，实际 %d 条：%+v", errorsAtLevel, logs())
	}
}

func TestHandleOpportunityWon_业务拒绝不记ERROR(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	orch := New(r, startUpstreamStubs(t), testWarehouseID, 5*time.Second, "")
	logger, logs := newCaptureLogger()

	// 额度 0：确认这一步因超额度被拒（业务结果，交给商机负责人的异常待办处理）
	customerID := uniqueSuffix("C-won-loglevel")
	seedCustomerSnapshot(t, db, customerID, "0")
	orch.HandleOpportunityWon(context.Background(), OpportunityWonPayload{
		OpportunityID: uniqueSuffix("opp-won-loglevel"), CustomerID: customerID, OwnerID: "u_won_loglevel",
		DeptPath: "/won-loglevel/", Currency: "CNY",
		Items: []OpportunityWonItem{{ProductID: uniqueSuffix("P-won-loglevel"), Quantity: "1", QuotedUnitPrice: "1.00"}},
	}, logger)

	found := false
	for _, rec := range logs() {
		if rec.msg == "商机赢单自动确认订单失败" {
			found = true
			if rec.level >= slog.LevelError {
				t.Fatalf("超额度是业务拒绝，不该记 ERROR，实际 %s", rec.level)
			}
		}
	}
	if !found {
		t.Fatalf("应记下确认失败，实际记录：%+v", logs())
	}
}
