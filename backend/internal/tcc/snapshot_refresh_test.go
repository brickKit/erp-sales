package tcc

// 额度快照不能只靠事件：mdm.customer.* 走的是 NATS 核心订阅，不重放。本组件
// 装进一个已经有客户的项目时，那些客户的 created 事件早就发完了；本组件停机
// 期间的 updated 事件也收不到。确认订单的第①步本来就向 mdm/customer 要这个
// 客户（BatchGet 带 credit_limit 与 version），额度要以它为准刷新快照
// （按 version 单调，不盖掉更新的事件值）。
//
// 这三条都是跨组件测试（requireE2EEnv）：本测试进程不跑事件消费循环，
// 测试库里没有快照，正好就是"客户比本组件先有"的真实处境。

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/brickKit/erp-sales/v2/backend/internal/repo"

	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
)

func snapshotLimit(t *testing.T, r *repo.Repo, customerID string) string {
	t.Helper()
	limit, _, err := r.GetCustomerSnapshot(context.Background(), customerID)
	if err != nil {
		t.Fatalf("读额度快照失败: %v", err)
	}
	return limit
}

func TestConfirmOrder_真实没有额度快照时按mdm返回的额度判(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	rdb := realDB(t)
	ctx := e2eTestCtx()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00") // 不写快照：没收到过 created 事件
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, rdb, productID, "50")

	order := createDraftOrder(t, ctx, orch, customerID, productID, "3")
	confirmed, err := orch.ConfirmOrder(ctx, order.ID, uniqueSuffix("test-confirm"), slog.Default())
	if err != nil {
		t.Fatalf("mdm/customer 给的额度是 100000.00，本单 %s，应该确认成功，实际：%v", order.TotalAmount, err)
	}
	if confirmed.Status != repo.StatusConfirmed {
		t.Fatalf("期望 CONFIRMED，实际 %q", confirmed.Status)
	}
	if got := snapshotLimit(t, orch.Repo, customerID); got != "100000.00" {
		t.Fatalf("确认之后快照应按 mdm/customer 的额度补上 100000.00，实际 %q", got)
	}
}

func TestConfirmOrder_真实mdm调低额度而快照没收到事件时按新额度拒绝(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	rdb := realDB(t)
	ctx := e2eTestCtx()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00")
	seedCustomerSnapshot(t, db, customerID, "100000.00") // 快照停在 version 1
	conn, closeConn, err := testClients().Customer(ctx)
	if err != nil {
		t.Fatalf("拨号 mdm-customer 失败: %v", err)
	}
	defer closeConn()
	if _, err := conn.Update(ctx, &customerv1.UpdateRequest{
		IdempotencyKey: uniqueSuffix("test-customer-update"), Id: customerID, Version: 1,
		Name: "TCC 测试客户", CreditLimit: "1.00",
	}); err != nil { // version 2；这条 updated 事件本测试进程收不到
		t.Fatalf("mdm-customer.Update 失败: %v", err)
	}
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, rdb, productID, "50")

	order := createDraftOrder(t, ctx, orch, customerID, productID, "3")
	_, err = orch.ConfirmOrder(ctx, order.ID, uniqueSuffix("test-confirm"), slog.Default())
	if err == nil {
		t.Fatalf("mdm/customer 已把额度调到 1.00，本单 %s 应该被拒，实际确认成功（用了快照里过时的 100000.00）", order.TotalAmount)
	}
	if got := snapshotLimit(t, orch.Repo, customerID); got != "1.00" {
		t.Fatalf("快照应按 mdm/customer 的 version 2 刷新成 1.00，实际 %q", got)
	}
}

func TestHandleOpportunityWon_真实没有额度快照时按mdm返回的额度判(t *testing.T) {
	requireE2EEnv(t)
	db := testDB(t)
	rdb := realDB(t)
	ctx := e2eTestCtx()
	orch := newTestOrchestrator(db, 5*time.Second)

	customerID := createRealCustomer(t, ctx, "100000.00") // 不写快照
	productID := createRealProduct(t, ctx)
	receiveRealStock(t, ctx, rdb, productID, "50")

	orch.HandleOpportunityWon(context.Background(), OpportunityWonPayload{
		OpportunityID: uniqueSuffix("test-opp-won"), CustomerID: customerID,
		Items:   []OpportunityWonItem{{ProductID: productID, Quantity: "3", QuotedUnitPrice: "999.00"}},
		OwnerID: "u_won_test_owner", DeptPath: "/1/12/", Currency: "CNY", Revision: 1,
	}, slog.Default())

	var status string
	if err := db.QueryRowContext(context.Background(), `
		SELECT status FROM erp_sales.sales_orders WHERE customer_id = $1 ORDER BY id DESC LIMIT 1`, customerID,
	).Scan(&status); err != nil {
		t.Fatalf("查订单失败（期望真的建出一条订单）: %v", err)
	}
	if status != repo.StatusConfirmed {
		t.Fatalf("mdm/customer 给的额度是 100000.00，赢单转出的订单应该是 CONFIRMED，实际 %q", status)
	}
}
