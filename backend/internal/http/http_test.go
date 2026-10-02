package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/client"
	"github.com/brickKit/erp-sales/v2/backend/internal/repo"
	"github.com/brickKit/erp-sales/v2/backend/internal/service"
	"github.com/brickKit/erp-sales/v2/backend/internal/tcc"
)

// 这里测的是 REST 层把查询参数接对了：handler 直接挂在测试自己的 engine 上，
// 不经过权限中间件（权限判定是 SDK 的事，由 SDK 自己的测试守）；调用者身份
// 由测试中间件塞进 ctx（生产环境由 SDK 的鉴权中间件从 JWT 里取）。

var seq int64

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddInt64(&seq, 1))
}

func testRepo(t *testing.T) (*repo.Repo, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return repo.New(db, "erp_sales_rw", "erp_sales"), db
}

func testService(r *repo.Repo) *service.Service {
	orch := tcc.New(r, client.New(besdk.NewConfig(nil)), "1", time.Second, "")
	return service.New(r, orch, slog.Default())
}

// newEngine 挂上本组件的全部路由（不带权限中间件），并把 claims 塞进每个请求的 ctx。
func newEngine(svc *service.Service, claims besdk.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(besdk.ContextWithClaims(c.Request.Context(), claims))
		c.Next()
	})
	g := eng.Group("/erp/sales")
	g.GET("/orders", listOrdersHandler(svc))
	g.GET("/stats/summary", statsSummaryHandler(svc))
	return eng
}

func do(t *testing.T, eng *gin.Engine, method, path string) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code, w.Body.Bytes()
}

type listBody struct {
	Orders []struct {
		ID string `json:"id"`
	} `json:"orders"`
}

func listIDs(t *testing.T, eng *gin.Engine, q url.Values) (int, map[string]bool) {
	t.Helper()
	code, body := do(t, eng, http.MethodGet, "/erp/sales/orders?"+q.Encode())
	ids := map[string]bool{}
	if code == http.StatusOK {
		var lb listBody
		if err := json.Unmarshal(body, &lb); err != nil {
			t.Fatalf("响应不是合法 JSON：%v（%s）", err, body)
		}
		for _, o := range lb.Orders {
			ids[o.ID] = true
		}
	}
	return code, ids
}

func createOrder(t *testing.T, r *repo.Repo, deptPath, owner string) *repo.Order {
	t.Helper()
	o, err := r.CreateOrder(context.Background(), repo.CreateOrderInput{
		IdempotencyKey: uniq("http-create"), CustomerID: "C-http", CustomerName: "REST 测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-http", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "10.00", Discount: "0", TaxRate: "0", Subtotal: "10.00"}},
		DeptPath: deptPath, OwnerID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// 契约里 GET /orders 带 created_after / created_before：调用方收窄时间窗口，
// 窗口外的订单就不该出现。
func TestListOrders_REST读取created_after与created_before(t *testing.T) {
	r, _ := testRepo(t)
	dept := "/" + uniq("dept") + "/"
	o := createOrder(t, r, dept, uniq("u"))
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: dept})

	code, ids := listIDs(t, eng, url.Values{})
	if code != http.StatusOK || !ids[o.ID] {
		t.Fatalf("不带时间参数应该看到刚建的订单：code=%d ids=%v", code, ids)
	}

	before := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	code, ids = listIDs(t, eng, url.Values{"created_before": {before}})
	if code != http.StatusOK {
		t.Fatalf("created_before 合法时应返回 200，实际 %d", code)
	}
	if ids[o.ID] {
		t.Fatalf("created_before=%s 时，刚建的订单 %s 不该出现", before, o.ID)
	}

	after := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, ids = listIDs(t, eng, url.Values{"created_after": {after}})
	if code != http.StatusOK {
		t.Fatalf("created_after 合法时应返回 200，实际 %d", code)
	}
	if ids[o.ID] {
		t.Fatalf("created_after=%s 时，刚建的订单 %s 不该出现", after, o.ID)
	}
}

func TestListOrders_REST时间参数格式不对返回400(t *testing.T) {
	r, _ := testRepo(t)
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: "/1/"})
	for _, name := range []string{"created_after", "created_before"} {
		code, _ := do(t, eng, http.MethodGet, "/erp/sales/orders?"+name+"=yesterday")
		if code != http.StatusBadRequest {
			t.Fatalf("%s 不是 RFC 3339 时间应返回 400，实际 %d", name, code)
		}
	}
}

// GET /orders?source_opportunity_id= 只返回由这张商机转成的订单，响应里带
// source_opportunity_id（商机详情页据此反查订单）。
func TestListOrders_REST按来源商机过滤且返回该字段(t *testing.T) {
	r, _ := testRepo(t)
	dept := "/" + uniq("dept") + "/"
	opp := uniq("opp")
	ctx := context.Background()
	won, err := r.CreateOrder(ctx, repo.CreateOrderInput{
		IdempotencyKey: uniq("http-won"), CustomerID: "C-http", CustomerName: "REST 测试客户",
		Items: []repo.CreateOrderItemInput{{ProductID: "P-http", ProductSKU: "SKU", ProductName: "N", UOMID: "EA",
			Qty: "1", UnitPrice: "10.00", Discount: "0", TaxRate: "0", Subtotal: "10.00"}},
		DeptPath: dept, OwnerID: "u_http", SourceOpportunityID: opp,
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := createOrder(t, r, dept, "u_http")
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: dept})

	code, body := do(t, eng, http.MethodGet, "/erp/sales/orders?"+url.Values{"source_opportunity_id": {opp}}.Encode())
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（%s）", code, body)
	}
	var lb struct {
		Orders []struct {
			ID                  string `json:"id"`
			SourceOpportunityID string `json:"source_opportunity_id"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(body, &lb); err != nil {
		t.Fatal(err)
	}
	if len(lb.Orders) != 1 || lb.Orders[0].ID != won.ID || lb.Orders[0].SourceOpportunityID != opp {
		t.Fatalf("期望只返回订单 %s（source_opportunity_id=%s），不含手工单 %s，实际 %+v", won.ID, opp, manual.ID, lb.Orders)
	}
}

type statsBody struct {
	Period         string         `json:"period"`
	From           string         `json:"from"`
	To             string         `json:"to"`
	OrderCount     int            `json:"order_count"`
	ConfirmedCount int            `json:"confirmed_count"`
	TotalAmount    string         `json:"total_amount"`
	ByStatus       map[string]int `json:"by_status"`
	ByDay          []struct {
		Date        string `json:"date"`
		OrderCount  int    `json:"order_count"`
		TotalAmount string `json:"total_amount"`
	} `json:"by_day"`
}

func getStats(t *testing.T, eng *gin.Engine, q url.Values) (int, statsBody) {
	t.Helper()
	code, body := do(t, eng, http.MethodGet, "/erp/sales/stats/summary?"+q.Encode())
	var sb statsBody
	if code == http.StatusOK {
		if err := json.Unmarshal(body, &sb); err != nil {
			t.Fatalf("响应不是合法 JSON：%v（%s）", err, body)
		}
	}
	return code, sb
}

// 统计按调用者的数据范围过滤，view 与列表同义：dept（默认）看部门前缀，mine 看 owner。
// 两个不同范围的用户看到不同的数。
func TestStatsSummary_REST按调用者范围与view统计(t *testing.T) {
	r, _ := testRepo(t)
	deptA := "/" + uniq("stats-a") + "/"
	deptB := "/" + uniq("stats-b") + "/"
	ownerA := uniq("u_a")
	for i := 0; i < 2; i++ {
		createOrder(t, r, deptA, ownerA)
	}
	createOrder(t, r, deptB, uniq("u_b"))
	createOrder(t, r, deptB, ownerA)
	svc := testService(r)

	code, a := getStats(t, newEngine(svc, besdk.Claims{Sub: ownerA, DeptPath: deptA}), url.Values{})
	if code != http.StatusOK || a.Period != "month" || len(a.ByDay) != 30 || a.OrderCount != 2 || len(a.ByStatus) != 7 {
		t.Fatalf("A 默认（dept、month）应看到本部门 2 单、30 天、7 个状态，实际 code=%d %+v", code, a)
	}
	if a.ByStatus["DRAFT"] != 2 || a.ConfirmedCount != 0 || a.TotalAmount != "0.00" {
		t.Fatalf("A 的 2 单都是草稿：期望 DRAFT=2、confirmed_count=0、total_amount=0.00，实际 %+v", a)
	}
	_, b := getStats(t, newEngine(svc, besdk.Claims{Sub: uniq("u_other"), DeptPath: deptB}), url.Values{})
	if b.OrderCount != 2 {
		t.Fatalf("B 部门的人应只看到 B 部门的 2 单，实际 %d", b.OrderCount)
	}
	_, mine := getStats(t, newEngine(svc, besdk.Claims{Sub: ownerA, DeptPath: deptA}), url.Values{"view": {"mine"}, "period": {"week"}})
	if mine.OrderCount != 3 || mine.Period != "week" || len(mine.ByDay) != 7 {
		t.Fatalf("A 用 view=mine、period=week 应看到自己名下 3 单（含 B 部门那单）、7 天，实际 %+v", mine)
	}
}

func TestStatsSummary_REST不认识的period返回400(t *testing.T) {
	r, _ := testRepo(t)
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: "/1/"})
	if code, _ := getStats(t, eng, url.Values{"period": {"year"}}); code != http.StatusBadRequest {
		t.Fatalf("period=year 应返回 400，实际 %d", code)
	}
}
