// sales_orders/sales_order_items 的月分区维护——订单是纯日历滚动窗口
// （同 erp-inventory 的月分区判据），复用 partition.go 的 ensurePartition
// （分区名格式必须和这里生成的一致，同 erp-inventory 设计计划 §9 第 8
// 条的教训：迁移里的初始分区名与这里生成的名字必须共用同一套格式）。
//
// ⚠️ sales_order_items 用**订单头的** created_at 做分区键（列名
// order_created_at，设计计划 §2、§7 子表跟随主表）——这里两张表用同一套
// 时间边界一起建，保证子表分区总是和头表分区同步存在。
package partition

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

const lookAheadMonths = 3 // 提前建好当前月 + 未来 3 个月

var monthlyPartitionedTables = []string{"sales_orders", "sales_order_items"}

func StartMonthly(ctx context.Context, db *sql.DB, role, schema string, logger *slog.Logger) error {
	if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
		logger.Error("月分区维护失败", "error", err)
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := ensureAllMonthly(ctx, db, role, schema); err != nil {
				logger.Error("月分区维护失败", "error", err)
			}
		}
	}
}

func ensureAllMonthly(ctx context.Context, db *sql.DB, role, schema string) error {
	return besdk.WithTx(ctx, db, role, schema, func(tx *sql.Tx) error {
		monthStart := firstOfMonth(time.Now().UTC())
		for i := 0; i <= lookAheadMonths; i++ {
			from := monthStart.AddDate(0, i, 0)
			to := from.AddDate(0, 1, 0)
			for _, table := range monthlyPartitionedTables {
				if err := ensurePartition(ctx, tx, table, from, to); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
