// Package consumer 消费 finance.credit.rejected.v1（权威额度超限→订单
// 转 SUSPENDED）+ mdm.customer.created.v1/.updated.v1（维护
// customer_snapshots.credit_limit）。⚠️ 不消费 finance.voucher.posted.v1
// ——它没有 customer_id，credit_exposure 的新鲜度靠定时对账（设计计划
// §9 第 6 条）。crm.opportunity.won.v1 阶段二只在事件清单占位，不订阅。
package consumer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/nats-io/nats.go"

	"github.com/brickKit/erp-sales/backend/internal/repo"
)

func Start(ctx context.Context, db *sql.DB, role, schema string, nc *nats.Conn, logger *slog.Logger) error {
	subjects := []struct {
		subject string
		handle  func(context.Context, *sql.Tx, besdk.Event) error
	}{
		{"finance.credit.rejected.v1", creditRejectedHandler()},
		{"mdm.customer.created.v1", customerSnapshotHandler()},
		{"mdm.customer.updated.v1", customerSnapshotHandler()},
	}

	errCh := make(chan error, len(subjects))
	for _, s := range subjects {
		s := s
		go func() {
			errCh <- besdk.Consume(ctx, nc, db, role, schema, s.subject, s.handle)
		}()
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err // ⚠️ 返回 error，不许 log.Fatal（§13.3 铁律七）
	}
}

// creditRejectedPayload 字段直接照抄 erp-finance 已经真实存在的契约
// （erp-finance Task 12，contracts/events/finance.events.json）。
type creditRejectedPayload struct {
	CustomerID string `json:"customer_id"`
	OrderID    string `json:"order_id"`
	Exposure   string `json:"exposure"`
	Limit      string `json:"limit"`
}

func creditRejectedHandler() func(context.Context, *sql.Tx, besdk.Event) error {
	return func(ctx context.Context, tx *sql.Tx, ev besdk.Event) error {
		var p creditRejectedPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		orderID, err := strconv.ParseInt(p.OrderID, 10, 64)
		if err != nil {
			return fmt.Errorf("解析 order_id: %w", err)
		}
		return repo.SuspendOrderTx(ctx, tx, orderID,
			fmt.Sprintf("权威额度超限：已用 %s，额度 %s", p.Exposure, p.Limit))
	}
}

type customerPayload struct {
	ID          string `json:"id"`
	CreditLimit string `json:"credit_limit"`
}

func customerSnapshotHandler() func(context.Context, *sql.Tx, besdk.Event) error {
	return func(_ context.Context, tx *sql.Tx, ev besdk.Event) error {
		var p customerPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return fmt.Errorf("解析 %s payload: %w", ev.Subject, err)
		}
		return repo.UpsertCustomerSnapshotCreditLimitTx(tx, p.ID, p.CreditLimit, ev.Version)
	}
}
