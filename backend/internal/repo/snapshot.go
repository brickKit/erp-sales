package repo

import (
	"database/sql"
	"fmt"
	"regexp"
)

// creditLimitPattern 是快照接受的额度写法：非负十进制数，整数最多 16 位、
// 小数最多 2 位，正好装进 customer_snapshots.credit_limit 的 NUMERIC(18,2)。
// "NaN"、"Infinity"、"1e3"、"0x10" 之类 PostgreSQL 也认的写法一律不收：
// NaN 落进 NUMERIC 后排在一切数字之上，负数让这个客户的每一单都超额度。
var creditLimitPattern = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)

// UpsertCustomerSnapshotCreditLimitTx 维护 mdm/customer 客户额度的本地副本，
// 只取 credit_limit 一列。在 besdk.Consume 给的事务里调用（接 *sql.Tx，不自己
// 开 besdk.WithTx：那会另开一个事务，与 Consume 已经打开的那个冲突）。
//
// creditLimit 为空表示事件没带这个字段（updated.v1 里它不是必填）：额度不变，
// 只推进 version；从没见过的客户按额度 0 建快照。写法不合法时返回
// ErrInvalidArgument，快照不动。
//
// WHERE version < EXCLUDED.version：按 version 单调更新，晚到的旧事件不覆盖
// 新值。credit_exposure 不在这里改：它与 credit_limit 的来源不同，不能混在同
// 一次 UPSERT 里写。
func UpsertCustomerSnapshotCreditLimitTx(tx *sql.Tx, customerID, creditLimit string, version int64) error {
	setLimit := "credit_limit = EXCLUDED.credit_limit, "
	if creditLimit == "" {
		creditLimit, setLimit = "0", ""
	} else if !creditLimitPattern.MatchString(creditLimit) {
		return fmt.Errorf("%w: credit_limit 必须是非负十进制数（整数最多 16 位、小数最多 2 位）：%q", ErrInvalidArgument, creditLimit)
	}
	_, err := tx.Exec(`
		INSERT INTO customer_snapshots (customer_id, credit_limit, version)
		VALUES ($1, $2, $3)
		ON CONFLICT (customer_id) DO UPDATE
		   SET `+setLimit+`version = EXCLUDED.version, updated_at = now()
		 WHERE customer_snapshots.version < EXCLUDED.version`,
		customerID, creditLimit, version)
	if err != nil {
		return fmt.Errorf("写 customer_snapshots: %w", err)
	}
	return nil
}

// GetCustomerSnapshotTx 读客户额度副本——确认订单时本地信用额度预判用（不走
// 网络）。查不到视为额度 0、已用额度 0，不报错：额度 0 时任何正金额的订单都
// 会被拒，要等 mdm.customer.* 事件把额度带过来。
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
