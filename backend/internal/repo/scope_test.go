// 阶段三 Task 6：org/owner 两维数据范围真实生效——对应设计计划 §1
// "我的订单"和"本部门及下级的订单"两个视图。
package repo

import (
	"context"
	"testing"
)

func TestOrder_InScope(t *testing.T) {
	cases := []struct {
		name        string
		deptPath    string
		ownerID     string
		scopePrefix string
		scopeOwner  string
		want        bool
	}{
		{"部门前缀命中", "/1/12/", "u_zhangsan", "/1/12/", "u_someone_else", true},
		{"部门前缀命中下级", "/1/12/34/", "u_zhangsan", "/1/12/", "u_someone_else", true},
		{"owner精确命中", "/1/99/", "u_zhangsan", "/1/12/", "u_zhangsan", true},
		{"两个都不命中", "/1/99/", "u_zhangsan", "/1/12/", "u_someone_else", false},
		{"根节点前缀天然命中全部", "/1/99/", "u_zhangsan", "", "u_someone_else", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := &Order{DeptPath: c.deptPath, OwnerID: c.ownerID}
			if got := o.InScope(c.scopePrefix, c.scopeOwner); got != c.want {
				t.Errorf("InScope(%q, %q) with order{DeptPath:%q OwnerID:%q} = %v，期望 %v",
					c.scopePrefix, c.scopeOwner, c.deptPath, c.ownerID, got, c.want)
			}
		})
	}
}

// TestListOrders_org维前缀过滤 与 TestListOrders_owner维过滤 是数据范围
// 真实生效的核心断言：授权范围必须下推进 SQL 的 WHERE，不能查出全部
// 结果后在 Go 里再过滤（决策 53 的既有判据）。
func TestListOrders_org维前缀过滤(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")

	east, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("scope-east"), CustomerID: "C-scope", CustomerName: "测试客户",
		Items: testItems(), DeptID: "12", DeptPath: "/1/12/", OwnerID: "u_east_rep",
	})
	if err != nil {
		t.Fatal(err)
	}
	south, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("scope-south"), CustomerID: "C-scope", CustomerName: "测试客户",
		Items: testItems(), DeptID: "34", DeptPath: "/1/34/", OwnerID: "u_south_rep",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 只授权 /1/12/ 这条前缀——应该只看到东区的订单。
	res, err := r.ListOrders(ctx, ListInput{PageSize: 200, ScopePrefix: "/1/12/"})
	if err != nil {
		t.Fatal(err)
	}
	foundEast, foundSouth := false, false
	for _, o := range res.Orders {
		if o.ID == east.ID {
			foundEast = true
		}
		if o.ID == south.ID {
			foundSouth = true
		}
	}
	if !foundEast {
		t.Fatal("授权东区前缀后应该能看到东区订单")
	}
	if foundSouth {
		t.Fatal("授权东区前缀不该看到南区订单")
	}
}

func TestListOrders_owner维过滤(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r := New(db, "erp_sales_rw", "erp_sales")
	ownerA := "u_" + uniqueSuffix("owner-a")
	ownerB := "u_" + uniqueSuffix("owner-b")

	mine, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("scope-mine"), CustomerID: "C-scope", CustomerName: "测试客户",
		Items: testItems(), DeptID: "1", DeptPath: "/1/", OwnerID: ownerA,
	})
	if err != nil {
		t.Fatal(err)
	}
	notMine, err := r.CreateOrder(ctx, CreateOrderInput{
		IdempotencyKey: uniqueSuffix("scope-notmine"), CustomerID: "C-scope", CustomerName: "测试客户",
		Items: testItems(), DeptID: "1", DeptPath: "/1/", OwnerID: ownerB,
	})
	if err != nil {
		t.Fatal(err)
	}

	// ViewMine=true 时只用 ScopeOwner，不管 ScopePrefix——即使两张订单
	// 部门路径相同（都在 /1/），"我的订单"也只看 owner_id。
	res, err := r.ListOrders(ctx, ListInput{PageSize: 200, ViewMine: true, ScopeOwner: ownerA, ScopePrefix: "/1/"})
	if err != nil {
		t.Fatal(err)
	}
	foundMine, foundNotMine := false, false
	for _, o := range res.Orders {
		if o.ID == mine.ID {
			foundMine = true
		}
		if o.ID == notMine.ID {
			foundNotMine = true
		}
	}
	if !foundMine {
		t.Fatal("ViewMine 应该能看到自己的订单")
	}
	if foundNotMine {
		t.Fatal("ViewMine 不该看到别人的订单，即使部门路径相同")
	}
}
