package repo

import (
	"database/sql"
	"fmt"
)

// UpsertCustomerSnapshotCreditLimitTx 维护 mdm-customer 的摘要副本——只
// 取 credit_limit（额度值本身，设计计划 §5 的三方分工）。供
// backend/internal/consumer 在 besdk.Consume 给的事务里调用（同
// erp-inventory/erp-finance 的既有判据：接 *sql.Tx 不自己开
// besdk.WithTx，那会开一个新事务和 Consume 已经打开的那个冲突）。
//
// ⚠️ WHERE version < $2 是按 version 单调更新（§3.10）。credit_exposure
// 这一列不在这里动——它靠定时对账维护，不消费事件（设计计划 §9 第 6 条），
// 两列的更新来源完全不同，不能用同一次 UPSERT 混着写。
func UpsertCustomerSnapshotCreditLimitTx(tx *sql.Tx, customerID, creditLimit string, version int64) error {
	if creditLimit == "" {
		creditLimit = "0"
	}
	_, err := tx.Exec(`
		INSERT INTO customer_snapshots (customer_id, credit_limit, version)
		VALUES ($1, $2, $3)
		ON CONFLICT (customer_id) DO UPDATE
		   SET credit_limit = EXCLUDED.credit_limit, version = EXCLUDED.version, updated_at = now()
		 WHERE customer_snapshots.version < EXCLUDED.version`,
		customerID, creditLimit, version)
	if err != nil {
		return fmt.Errorf("写 customer_snapshots: %w", err)
	}
	return nil
}

// GetCustomerSnapshotTx 读客户摘要副本——本地免费信用校验用（ConfirmOrder
// 的 TCC 链第③步，设计计划 §3.1）。查不到视为额度 0、已用额度 0，不报错
// ——还没消费到事件不该让下单流程失败。
func GetCustomerSnapshotTx(tx *sql.Tx, customerID string) (creditLimit, creditExposure string, err error) {
	err = tx.QueryRow(`SELECT credit_limit, credit_exposure FROM customer_snapshots WHERE customer_id = $1`,
		customerID).Scan(&creditLimit, &creditExposure)
	if err == sql.ErrNoRows {
		return "0", "0", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("查 customer_snapshots: %w", err)
	}
	return creditLimit, creditExposure, nil
}
