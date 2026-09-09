package tcc

import (
	"context"
	"fmt"
	"strings"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/backend/internal/repo"
)

// leafDeptID 从 dept_path 的末段推出 dept_id（阶段三 Task 6）。
//
// ⚠️ 这不是凭空发明的格式：infra-authz 的极简部门表（阶段三唯一的真实
// dept_path 来源，departments.go 的 CreateDepartment）本身就是这么算
// dept_path 的——`parentPath + strconv.FormatInt(id, 10) + "/"`，所以
// "/1/12/" 这条路径的末段 "12" 恰好就是这个部门自己的 id，不需要额外
// 一次查询或者往 JWT 里加一个新字段。dept_path 为空（根节点）时
// dept_id 也是空——两者都表示"没有具体部门"，同一个意思，不用特判。
//
// 只在本组件内联，不提这进 be-sdk-go：现在只有这一个调用方，SOP-P 的
// 判据是"逻辑本身简单却硬套抽象是更糟的结果"——等阶段四/五出现第二个
// 真的需要它的组件（crm-*/hrm-*/prj-*），再把它提升成 SDK 里的公用
// 函数，现在提前抽象是没有第二个使用者验证过的猜测。
func leafDeptID(deptPath string) string {
	trimmed := strings.Trim(deptPath, "/")
	if trimmed == "" {
		return ""
	}
	segments := strings.Split(trimmed, "/")
	return segments[len(segments)-1]
}

// CreateOrderItemInput 是 CreateOrder 的入参一项——只有 product_id/qty，
// 其余快照字段（sku/name/uom/单价/折扣/税率/小计）都在这里解析出来
// （设计计划 §2.2：全部快照，不 join）。
type CreateOrderItemInput struct {
	ProductID string
	Qty       string
}

// CreateOrder 建 DRAFT，不预留库存（设计计划 §3 契约面）。没有需要补偿
// 的外部写，所以不是真正的 TCC，但同样要先校验客户/产品、再落库——
// 跨组件读调用与本地写分离的骨架和 ConfirmOrder 一致。
func (o *Orchestrator) CreateOrder(ctx context.Context, idempotencyKey, customerID string, items []CreateOrderItemInput) (*repo.Order, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", repo.ErrInvalidArgument)
	}

	customer, err := fetchActiveCustomer(ctx, customerID)
	if err != nil {
		return nil, err
	}

	productIDs := make([]string, 0, len(items))
	for _, it := range items {
		productIDs = append(productIDs, it.ProductID)
	}
	products, err := fetchActiveProducts(ctx, uniqueStrings(productIDs))
	if err != nil {
		return nil, err
	}

	priceInputs := make([]repo.PriceInput, 0, len(items))
	for _, it := range items {
		p := products[it.ProductID]
		priceInputs = append(priceInputs, repo.PriceInput{ProductID: it.ProductID, CategoryID: p.CategoryId, Qty: it.Qty})
	}
	priceResults, _, err := o.Repo.CalculatePriceDryRun(ctx, priceInputs)
	if err != nil {
		return nil, fmt.Errorf("定价失败: %w", err)
	}
	if len(priceResults) != len(items) {
		return nil, fmt.Errorf("定价结果数量（%d）与订单行数量（%d）不一致", len(priceResults), len(items))
	}

	createItems := make([]repo.CreateOrderItemInput, 0, len(items))
	for i, it := range items {
		p := products[it.ProductID]
		pr := priceResults[i]
		createItems = append(createItems, repo.CreateOrderItemInput{
			ProductID: it.ProductID, ProductSKU: p.Sku, ProductName: p.Name, UOMID: p.BaseUomId,
			Qty: it.Qty, UnitPrice: pr.UnitPrice, Discount: pr.Discount, TaxRate: pr.TaxRate, Subtotal: pr.Subtotal,
		})
	}

	// dept_id/dept_path/owner_id 是创建时快照（设计计划 §1）——阶段三
	// Task 6 从 besdk.ScopeOf(ctx) 取真实值，不再是阶段二的空字符串占位。
	scope := besdk.ScopeOf(ctx)
	return o.Repo.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: idempotencyKey, CustomerID: customerID, CustomerName: customer.Name, Items: createItems,
		DeptID: leafDeptID(scope.Prefix), DeptPath: scope.Prefix, OwnerID: scope.Owner,
	})
}
