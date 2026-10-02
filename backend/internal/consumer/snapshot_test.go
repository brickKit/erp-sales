package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// mdm.customer.created/updated.v1 的 credit_limit 是 decimal-as-string，
// 落进 customer_snapshots.credit_limit NUMERIC(18,2)，确认订单时拿它做本地
// 信用额度预判。规则：
//   - 只接受不带指数、非负的十进制写法。"NaN"、"Infinity" 之类 PostgreSQL 的
//     NUMERIC 照样收下（NaN 排在一切数字之上），负数让这个客户的每一单都
//     超额度——这类事件整条拒绝（handler 返回 error），快照保持原值；
//   - updated.v1 里 credit_limit 不是必填：没带（或是空串）表示"额度没变"，
//     保留原来的额度，不能清零。

func applyCustomerEvent(t *testing.T, db *sql.DB, version int64, payload string) error {
	t.Helper()
	ev := besdk.Event{Subject: "mdm.customer.updated.v1", Version: version, Payload: []byte(payload)}
	return besdk.WithTx(context.Background(), db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		return customerSnapshotHandler()(context.Background(), tx, ev)
	})
}

func creditLimitOf(t *testing.T, r *repo.Repo, customerID string) string {
	t.Helper()
	limit, _, err := r.GetCustomerSnapshot(context.Background(), customerID)
	if err != nil {
		t.Fatal(err)
	}
	return limit
}

func TestCustomerSnapshot_非法额度整条拒绝且不改快照(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	for i, bad := range []string{"NaN", "nan", "Infinity", "-Infinity", "inf", "1e3", "-1.00", "abc", "0x10"} {
		id := fmt.Sprintf("snap-bad-%d-%d", time.Now().UnixNano(), i)
		if err := applyCustomerEvent(t, db, 1, fmt.Sprintf(`{"id":%q,"credit_limit":"5000.00"}`, id)); err != nil {
			t.Fatalf("合法额度不该报错：%v", err)
		}
		err := applyCustomerEvent(t, db, 2, fmt.Sprintf(`{"id":%q,"credit_limit":%q}`, id, bad))
		if err == nil {
			t.Errorf("credit_limit=%q 不是非负十进制数，事件应被拒绝", bad)
		}
		if got := creditLimitOf(t, r, id); got != "5000.00" {
			t.Errorf("credit_limit=%q 的事件不该改快照：期望仍是 5000.00，实际 %s", bad, got)
		}
	}
}

func TestCustomerSnapshot_没带额度时保留原额度(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	id := fmt.Sprintf("snap-keep-%d", time.Now().UnixNano())
	if err := applyCustomerEvent(t, db, 1, fmt.Sprintf(`{"id":%q,"credit_limit":"8000.50"}`, id)); err != nil {
		t.Fatal(err)
	}
	if err := applyCustomerEvent(t, db, 2, fmt.Sprintf(`{"id":%q,"name":"改了名字"}`, id)); err != nil {
		t.Fatal(err)
	}
	if got := creditLimitOf(t, r, id); got != "8000.50" {
		t.Fatalf("没带 credit_limit 的 updated 事件不该改额度：期望 8000.50，实际 %s", got)
	}
	if err := applyCustomerEvent(t, db, 3, fmt.Sprintf(`{"id":%q,"credit_limit":""}`, id)); err != nil {
		t.Fatal(err)
	}
	if got := creditLimitOf(t, r, id); got != "8000.50" {
		t.Fatalf("credit_limit 为空串的 updated 事件不该改额度：期望 8000.50，实际 %s", got)
	}
	// 之后带了新额度照常更新（版本号一直在往前走）
	if err := applyCustomerEvent(t, db, 4, fmt.Sprintf(`{"id":%q,"credit_limit":"100"}`, id)); err != nil {
		t.Fatal(err)
	}
	if got := creditLimitOf(t, r, id); got != "100.00" {
		t.Fatalf("带了新额度应更新：期望 100.00，实际 %s", got)
	}
}

func TestCustomerSnapshot_新客户没带额度时额度为0(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	id := fmt.Sprintf("snap-new-%d", time.Now().UnixNano())
	if err := applyCustomerEvent(t, db, 1, fmt.Sprintf(`{"id":%q}`, id)); err != nil {
		t.Fatal(err)
	}
	if got := creditLimitOf(t, r, id); got != "0.00" {
		t.Fatalf("新客户没带额度时快照额度应为 0.00，实际 %s", got)
	}
}
