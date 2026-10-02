package repo

import (
	"context"
	"testing"
)

// 订单记住它由哪张商机转成（source_opportunity_id）：赢单路径写入，手工
// 建单为空串；sales.order.created.v1 的 payload 带上它，crm/opportunity
// 据此回填商机的 order_id；列表能按它过滤（商机详情页反查订单）。

func createWithSource(t *testing.T, r *Repo, source string) *Order {
	t.Helper()
	o, err := r.CreateOrder(context.Background(), CreateOrderInput{
		IdempotencyKey: uniqueSuffix("src-opp-create"), CustomerID: "C-src", CustomerName: "来源商机测试客户",
		Items: testItems(), DeptPath: "/src-opp/", OwnerID: "u_src", SourceOpportunityID: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestCreateOrder_记录来源商机且手工建单为空(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	opp := uniqueSuffix("opp")

	won := createWithSource(t, r, opp)
	if won.SourceOpportunityID != opp {
		t.Fatalf("赢单建的订单应记住来源商机 %s，实际 %q", opp, won.SourceOpportunityID)
	}
	reloaded, err := r.GetOrder(context.Background(), won.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SourceOpportunityID != opp {
		t.Fatalf("重新读出的订单应带来源商机 %s，实际 %q", opp, reloaded.SourceOpportunityID)
	}

	manual := createWithSource(t, r, "")
	if manual.SourceOpportunityID != "" {
		t.Fatalf("手工建单的来源商机应为空串，实际 %q", manual.SourceOpportunityID)
	}
}

func TestFinalizeConfirm_订单创建事件带来源商机(t *testing.T) {
	db := testDB(t)
	r := New(db, "erp_sales_rw", "erp_sales")
	ctx := context.Background()
	opp := uniqueSuffix("opp")

	for _, c := range []struct{ source, want string }{{opp, opp}, {"", ""}} {
		o := createWithSource(t, r, c.source)
		if _, err := r.FinalizeConfirm(ctx, FinalizeConfirmInput{
			IdempotencyKey: uniqueSuffix("src-opp-confirm"), OrderID: o.ID, ReservationID: "R-src",
		}); err != nil {
			t.Fatal(err)
		}
		var got *string
		if err := db.QueryRowContext(ctx, `
			SELECT payload->>'source_opportunity_id' FROM erp_sales.event_outbox
			WHERE subject = 'sales.order.created.v1' AND aggregate_id = $1`, o.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != c.want {
			t.Fatalf("订单 %s 的 sales.order.created.v1 payload 应带 source_opportunity_id=%q，实际 %v", o.ID, c.want, got)
		}
	}
}

func TestListOrders_按来源商机过滤(t *testing.T) {
	r := New(testDB(t), "erp_sales_rw", "erp_sales")
	opp := uniqueSuffix("opp")
	won := createWithSource(t, r, opp)
	other := createWithSource(t, r, "")

	res, err := r.ListOrders(context.Background(), ListInput{SourceOpportunityID: opp, ScopePrefix: "/src-opp/", PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Orders) != 1 || res.Orders[0].ID != won.ID {
		ids := []string{}
		for _, o := range res.Orders {
			ids = append(ids, o.ID)
		}
		t.Fatalf("按来源商机过滤应只返回订单 %s（不含 %s），实际 %v", won.ID, other.ID, ids)
	}
}
