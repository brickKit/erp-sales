package tcc

import (
	"context"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
)

// 没有部门的人可以建单，这张单只对他本人可见：快照里的 dept_path / dept_id
// 留空串（不能把 SDK 给的"无部门"哨兵写进行里，也不能从它推出 dept_id），
// owner_id 是本人。别的无部门的人、根标记 "/" 的人都看不到它。
func TestCreateOrder_无部门的人建单dept_path与dept_id留空不写哨兵(t *testing.T) {
	db := testDB(t)
	r := repo.New(db, "erp_sales_rw", "erp_sales")
	orch := New(r, startUpstreamStubs(t), testWarehouseID, 5*time.Second, "")
	me := uniqueSuffix("u_nodept_creator")
	customer := uniqueSuffix("C-nodept-create")
	ctx := besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: me, DeptPath: ""})

	order, err := orch.CreateOrder(ctx, uniqueSuffix("nodept-create"), customer,
		[]CreateOrderItemInput{{ProductID: uniqueSuffix("P-nodept"), Qty: "1"}})
	if err != nil {
		t.Fatalf("没有部门的人应该能建单，实际：%v", err)
	}
	var deptPath, deptID, owner string
	if err := db.QueryRowContext(context.Background(),
		`SELECT dept_path, dept_id, owner_id FROM erp_sales.sales_orders WHERE id = $1::bigint`, order.ID,
	).Scan(&deptPath, &deptID, &owner); err != nil {
		t.Fatal(err)
	}
	if deptPath != "" || deptID != "" {
		t.Fatalf("没有部门的人建的单 dept_path / dept_id 应该是空串，实际 %q / %q", deptPath, deptID)
	}
	if owner != me {
		t.Fatalf("owner_id 应该是建单人 %s，实际 %q", me, owner)
	}

	visible := func(c besdk.Claims) bool {
		s := besdk.ScopeOf(besdk.ContextWithClaims(context.Background(), c))
		return order.InScope(s.Prefix, s.Owner)
	}
	if !visible(besdk.Claims{Sub: me, DeptPath: ""}) {
		t.Fatal("建单人自己应该看得到这张单")
	}
	if visible(besdk.Claims{Sub: uniqueSuffix("u_nodept_other"), DeptPath: ""}) {
		t.Fatal("别的无部门的人不该看到这张单")
	}
	if visible(besdk.Claims{Sub: uniqueSuffix("u_root"), DeptPath: "/"}) {
		t.Fatal("根标记 \"/\" 的人不该看到 dept_path 为空的单")
	}

	// 别的无部门的人的 dept 视图：列表是空的，不是错误。
	other := besdk.ScopeOf(besdk.ContextWithClaims(context.Background(),
		besdk.Claims{Sub: uniqueSuffix("u_nodept_other"), DeptPath: ""}))
	res, err := r.ListOrders(context.Background(), repo.ListInput{PageSize: 50, CustomerID: customer, ScopePrefix: other.Prefix})
	if err != nil {
		t.Fatalf("别的无部门的人 dept 视图应该是空列表而不是错误，实际：%v", err)
	}
	if len(res.Orders) != 0 {
		t.Fatalf("别的无部门的人 dept 视图不该列出这张单，实际 %d 张", len(res.Orders))
	}
}
