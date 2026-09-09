package repo

import (
	"context"
	"database/sql"
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
)

func mustParseID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := parseID("order_id", s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func createDraftOrderForSuspendTest(t *testing.T, db *sql.DB) *Order {
	t.Helper()
	r := New(db, "erp_sales_rw", "erp_sales")
	order, err := r.CreateOrder(context.Background(), CreateOrderInput{
		IdempotencyKey: uniqueSuffix("suspend-create"), CustomerID: "C-suspend", CustomerName: "挂起测试客户",
		Items: testItems(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

// TestIncrementCompensationAttempts_满3次转SUSPENDED且reason精确 验证
// §4.4.4 的落地前提：ExceptionReasonCompensationFailed 这个常量真的被
// 写进了 suspended_reason——ResumeFromExceptionTx 的精确匹配依赖这一点。
func TestIncrementCompensationAttempts_满3次转SUSPENDED且reason精确(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	order := createDraftOrderForSuspendTest(t, db)

	for i := 0; i < 2; i++ {
		attempts, err := r.IncrementCompensationAttempts(ctx, order.ID)
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := r.GetOrder(ctx, order.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.Status != StatusDraft {
			t.Fatalf("第 %d 次失败不该转 SUSPENDED，实际 %q（attempts=%d）", attempts, reloaded.Status, attempts)
		}
	}

	attempts, err := r.IncrementCompensationAttempts(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("期望第 3 次调用后 attempts=3，实际 %d", attempts)
	}
	reloaded, err := r.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusSuspended {
		t.Fatalf("期望满 3 次后转 SUSPENDED，实际 %q", reloaded.Status)
	}
	if reloaded.SuspendedReason != ExceptionReasonCompensationFailed {
		t.Fatalf("期望 suspended_reason 精确等于 %q，实际 %q", ExceptionReasonCompensationFailed, reloaded.SuspendedReason)
	}
}

// TestResumeFromExceptionTx_精确匹配reason才恢复 是设计计划 §4.4.4 那条
// 判据的直接测试：两条 SUSPENDED 来源共用同一个 status 值，只有 reason
// 能区分，恢复必须精确匹配，不能只判 status。
func TestResumeFromExceptionTx_精确匹配reason才恢复(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	order := createDraftOrderForSuspendTest(t, db)

	// 先真的走 3 次补偿失败转 SUSPENDED（同上一条测试的前置状态）。
	for i := 0; i < 3; i++ {
		if _, err := r.IncrementCompensationAttempts(ctx, order.ID); err != nil {
			t.Fatal(err)
		}
	}

	var resumed bool
	err := besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		var e error
		resumed, e = ResumeFromExceptionTx(ctx, tx, mustParseID(t, order.ID))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("期望 reason 精确匹配时 resumed=true")
	}
	reloaded, err := r.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusDraft {
		t.Fatalf("期望恢复后回到 DRAFT，实际 %q", reloaded.Status)
	}
	if reloaded.CompensationAttempts != 0 {
		t.Fatalf("期望恢复后 compensation_attempts 清零，实际 %d", reloaded.CompensationAttempts)
	}
	if reloaded.SuspendedReason != "" {
		t.Fatalf("期望恢复后 suspended_reason 清空，实际 %q", reloaded.SuspendedReason)
	}
}

// TestResumeFromExceptionTx_reason不匹配时不恢复 覆盖"这张订单后来又被
// 权威额度判定重新标了 SUSPENDED"这个真实场景——不能被这条事件误恢复。
func TestResumeFromExceptionTx_reason不匹配时不恢复(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	order := createDraftOrderForSuspendTest(t, db)

	err := besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		return SuspendOrderTx(ctx, tx, mustParseID(t, order.ID), "权威额度超限：已用 90000.00，额度 50000.00")
	})
	if err != nil {
		t.Fatal(err)
	}

	var resumed bool
	err = besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		var e error
		resumed, e = ResumeFromExceptionTx(ctx, tx, mustParseID(t, order.ID))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("reason 是权威额度超限，不该被补偿异常的恢复逻辑处理，期望 resumed=false")
	}
	reloaded, err := r.GetOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusSuspended {
		t.Fatalf("不匹配时订单应该原样保持 SUSPENDED，实际 %q", reloaded.Status)
	}
}

// TestResumeFromExceptionTx_不是SUSPENDED时不恢复 覆盖幂等/边界情形：
// 一张仍是 DRAFT（从没被挂起过）的订单收到这条事件不该出错也不该被动。
func TestResumeFromExceptionTx_不是SUSPENDED时不恢复(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	order := createDraftOrderForSuspendTest(t, db)

	var resumed bool
	err := besdk.WithTx(ctx, db, "erp_sales_rw", "erp_sales", func(tx *sql.Tx) error {
		var e error
		resumed, e = ResumeFromExceptionTx(ctx, tx, mustParseID(t, order.ID))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("从没被挂起过的订单不该被恢复，期望 resumed=false")
	}
}
