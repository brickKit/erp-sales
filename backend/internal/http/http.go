// Package http 是 erp-sales 的 REST 面（对外路径前缀 /erp/sales，与
// assembly.yaml 的 edge_routes 一致）。不暴露 BatchGetOrder / GetOrderStatus：
// 那是组件间协议，不是人的操作。CalculatePriceDryRun 必须暴露：前端下单页要
// 实时试算，前端与 BFF 不许自己算钱。
//
// 每条路由都带 assembly.yaml 里声明的权限键注册。/price/dry-run 归 create
// 档：它不读写任何订单，但语义上是"准备建单"的一部分，不单独开一个键。
// org + owner 两维数据范围（"本部门及下级" / "我的订单"）的过滤在
// backend/internal/service 与 backend/internal/repo/order.go：都从
// besdk.ScopeOf(ctx) 取 Prefix / Owner；建单时把它们连同 dept_id 写进订单头，
// 作为创建时的快照。
package http

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/service"
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
	besdk.GET(g, "/stats/summary", "erp.sales.view", statsSummaryHandler(svc))
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
		"reservation_id": o.ReservationID, "source_opportunity_id": o.SourceOpportunityID, "version": o.Version,
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

// parseView 读 ?view=：dept（省略时的默认）或 mine，二选一。别的取值回 400——
// 拼错的 view 悄悄当成 dept，调用者以为在看"我的订单"，拿到的却是整个部门的数。
func parseView(c *gin.Context) (viewMine bool, err error) {
	switch v := c.Query("view"); v {
	case "", "dept":
		return false, nil
	case "mine":
		return true, nil
	default:
		return false, fmt.Errorf("view 只能是 dept / mine，实际 %q", v)
	}
}

// parseListInput 读 GET /orders 的查询参数。created_after / created_before
// 是 RFC 3339 时间，不传就由 repo 补默认窗口；格式不对返回错误（handler
// 回 400），不悄悄忽略。
func parseListInput(c *gin.Context) (repo.ListInput, error) {
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	in := repo.ListInput{
		Cursor: c.Query("cursor"), PageSize: pageSize,
		CustomerID: c.Query("customer_id"), StatusFilter: c.Query("status_filter"),
		SourceOpportunityID: c.Query("source_opportunity_id"),
	}
	viewMine, err := parseView(c)
	if err != nil {
		return in, err
	}
	in.ViewMine = viewMine
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"created_after", &in.CreatedAfter}, {"created_before", &in.CreatedBefore}} {
		raw := c.Query(p.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return in, fmt.Errorf("%s 不是 RFC 3339 时间：%q", p.name, raw)
		}
		*p.dst = t
	}
	return in, nil
}

// listOrdersHandler：?view=mine|dept 决定用 org 维还是 owner 维（两个独立的
// 视图，见 repo.ListInput 同名字段注释）。省略时默认
// dept——"本部门及下级"是更常见的默认视角（管理者打开列表页通常想看
// 团队全貌），"我的订单"是窄化的显式选择。
func listOrdersHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		in, err := parseListInput(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		out, err := svc.ListOrders(c.Request.Context(), in)
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

// statsSummaryHandler：GET /stats/summary?period=week|month|quarter&view=dept|mine。
// 不认识的 period / view 直接回 400；数据范围与 GET /orders 相同（view 二选一，
// 不是 OR）。
func statsSummaryHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		period := c.Query("period")
		switch period {
		case "", "week", "month", "quarter":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("period 只能是 week / month / quarter，实际 %q", period)})
			return
		}
		viewMine, err := parseView(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		st, err := svc.StatsSummary(c.Request.Context(), period, viewMine)
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		byDay := make([]gin.H, 0, len(st.ByDay))
		for _, d := range st.ByDay {
			byDay = append(byDay, gin.H{"date": d.Date, "order_count": d.OrderCount, "total_amount": d.TotalAmount})
		}
		c.JSON(http.StatusOK, gin.H{
			"period": st.Period, "from": st.From, "to": st.To,
			"order_count": st.OrderCount, "confirmed_count": st.ConfirmedCount, "total_amount": st.TotalAmount,
			"by_day": byDay, "by_status": st.ByStatus,
		})
	}
}
