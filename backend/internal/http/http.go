// Package http 是 erp-sales 的 REST 面（对外路径前缀 /erp/sales，与
// assembly.yaml 的 edge_routes 一致）。⚠️ 不暴露 BatchGetOrder/
// GetOrderStatus——组件间协议，不是人类操作。CalculatePriceDryRun
// **必须**暴露——前端下单页要实时试算（设计计划 §3、§8.4）。
//
// 阶段三 Task 6：权限键从阶段二的 besdk.Public 换成 assembly.yaml 里
// 声明的真实键。`/price/dry-run` 是"建单前试算"的一部分，归到 create
// 档——它不读写任何具体订单，纯计算，但语义上属于"准备创建订单"这个
// 动作，不单独开一个权限键。org+owner 两维数据范围（"本部门及下级"/
// "我的订单"）另见 scope.go 与 backend/internal/repo/order.go：ListOrders
// 走 besdk.ScopeOf(ctx) 的 Prefix/Owner 两个字段，CreateOrder 把它们
// 连同 dept_id 一起写进订单头做创建时快照（设计计划 §1）。
package http

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/backend/internal/repo"
	"github.com/brickKit/erp-sales/backend/internal/service"
)

func RegisterRoutes(eng *gin.Engine, svc *service.Service) {
	g := eng.Group("/erp/sales")
	besdk.POST(g, "/orders", "erp.sales.create", createOrderHandler(svc))
	besdk.GET(g, "/orders", "erp.sales.view", listOrdersHandler(svc))
	besdk.GET(g, "/orders/:id", "erp.sales.view", getOrderHandler(svc))
	besdk.POST(g, "/orders/:id/confirm", "erp.sales.confirm", confirmOrderHandler(svc))
	besdk.POST(g, "/orders/:id/cancel", "erp.sales.cancel", cancelOrderHandler(svc))
	besdk.POST(g, "/orders/:id/ship", "erp.sales.ship", shipOrderHandler(svc))
	besdk.POST(g, "/price/dry-run", "erp.sales.create", calculatePriceDryRunHandler(svc))
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

func toOrderDTO(o *repo.Order) gin.H {
	items := make([]gin.H, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, gin.H{
			"product_id": it.ProductID, "product_sku": it.ProductSKU, "product_name": it.ProductName,
			"uom_id": it.UOMID, "qty": it.Qty, "unit_price": it.UnitPrice, "discount": it.Discount,
			"tax_rate": it.TaxRate, "subtotal": it.Subtotal,
		})
	}
	return gin.H{
		"id": o.ID, "order_no": o.OrderNo, "customer_id": o.CustomerID, "customer_name": o.CustomerName,
		"status": o.Status, "items": items, "total_amount": o.TotalAmount,
		"dept_id": o.DeptID, "dept_path": o.DeptPath, "owner_id": o.OwnerID,
		"reservation_id": o.ReservationID, "version": o.Version,
		"created_at": o.CreatedAt.Format(rfc3339), "updated_at": o.UpdatedAt.Format(rfc3339),
	}
}

type createOrderItemDTO struct {
	ProductID string `json:"product_id" binding:"required"`
	Qty       string `json:"qty" binding:"required"`
}

type createOrderRequest struct {
	IdempotencyKey string               `json:"idempotency_key" binding:"required"`
	CustomerID     string               `json:"customer_id" binding:"required"`
	Items          []createOrderItemDTO `json:"items" binding:"required"`
}

func createOrderHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createOrderRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		items := make([]service.CreateOrderItem, 0, len(req.Items))
		for _, it := range req.Items {
			items = append(items, service.CreateOrderItem{ProductID: it.ProductID, Qty: it.Qty})
		}
		order, err := svc.CreateOrder(c.Request.Context(), req.IdempotencyKey, req.CustomerID, items)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toOrderDTO(order))
	}
}

type orderCommandRequest struct {
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
	Reason         string `json:"reason"`
}

func confirmOrderHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req orderCommandRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		order, err := svc.ConfirmOrder(c.Request.Context(), c.Param("id"), req.IdempotencyKey)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toOrderDTO(order))
	}
}

func cancelOrderHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req orderCommandRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		order, err := svc.CancelOrder(c.Request.Context(), c.Param("id"), req.IdempotencyKey, req.Reason)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toOrderDTO(order))
	}
}

func shipOrderHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req orderCommandRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		order, err := svc.ShipOrder(c.Request.Context(), c.Param("id"), req.IdempotencyKey)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toOrderDTO(order))
	}
}

func getOrderHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		order, err := svc.GetOrder(c.Request.Context(), c.Param("id"))
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toOrderDTO(order))
	}
}

// listOrdersHandler：?view=mine|dept 决定用 org 维还是 owner 维（设计
// 计划 §1 的两个视图，见 repo.ListInput 同名字段注释）。省略时默认
// dept——"本部门及下级"是更常见的默认视角（管理者打开列表页通常想看
// 团队全貌），"我的订单"是窄化的显式选择。
func listOrdersHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		out, err := svc.ListOrders(c.Request.Context(), repo.ListInput{
			Cursor: c.Query("cursor"), PageSize: pageSize,
			CustomerID: c.Query("customer_id"), StatusFilter: c.Query("status_filter"),
			ViewMine: c.Query("view") == "mine",
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		dtos := make([]gin.H, 0, len(out.Orders))
		for _, o := range out.Orders {
			dtos = append(dtos, toOrderDTO(o))
		}
		c.JSON(http.StatusOK, gin.H{"orders": dtos, "next_cursor": out.NextCursor})
	}
}

type priceDryRunItemDTO struct {
	ProductID  string `json:"product_id" binding:"required"`
	CategoryID string `json:"category_id"`
	Qty        string `json:"qty" binding:"required"`
}

type calculatePriceDryRunRequest struct {
	CustomerID string               `json:"customer_id"`
	Items      []priceDryRunItemDTO `json:"items" binding:"required"`
}

func calculatePriceDryRunHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req calculatePriceDryRunRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		in := make([]repo.PriceInput, 0, len(req.Items))
		for _, it := range req.Items {
			in = append(in, repo.PriceInput{ProductID: it.ProductID, CategoryID: it.CategoryID, Qty: it.Qty})
		}
		results, total, err := svc.CalculatePriceDryRun(c.Request.Context(), in)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		items := make([]gin.H, 0, len(results))
		for _, r := range results {
			items = append(items, gin.H{
				"product_id": r.ProductID, "unit_price": r.UnitPrice, "discount": r.Discount,
				"tax_rate": r.TaxRate, "subtotal": r.Subtotal,
			})
		}
		c.JSON(http.StatusOK, gin.H{"items": items, "total_amount": total})
	}
}
