package http

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace/noop"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/internal/service"
)

// testRuntime 是 besdk.NewGinEngine 要的最小 Runtime：错误到状态码的映射、panic
// 恢复都在 SDK 的中间件里，REST 的状态码要经过它才是真实的。
func testRuntime() *besdk.Runtime {
	return &besdk.Runtime{
		Registry: besdk.NewRegistry(),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Tracer:   noop.NewTracerProvider().Tracer("test"),
	}
}

// sdkEngine 用 SDK 的真实 engine 挂单张订单的读与确认（不带权限键：验签与权限
// 判定是 SDK 的事），调用者身份由测试中间件塞进 ctx。
func sdkEngine(svc *service.Service, claims besdk.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	eng := besdk.NewGinEngine(testRuntime())
	asCaller := func(c *gin.Context) {
		c.Request = c.Request.WithContext(besdk.ContextWithClaims(c.Request.Context(), claims))
		c.Next()
	}
	g := eng.Group("/erp/sales", asCaller)
	g.GET("/orders/:id", getOrderHandler(svc))
	g.POST("/orders/:id/confirm", confirmOrderHandler(svc))
	return eng
}

// 范围外的单张读取答 404，和 id 根本不存在时一样，不泄露"这张单存在"；
// 对范围外的单做动作（确认）答 403。
func TestGetOrder_REST范围外答404与不存在一样_确认答403(t *testing.T) {
	r, _ := testRepo(t)
	svc := testService(r)
	other := createOrder(t, r, "/1/12/", uniq("u_owner"))

	for _, claims := range []besdk.Claims{
		{Sub: uniq("u_stranger"), DeptPath: "/9/99/"},
		{Sub: uniq("u_nodept"), DeptPath: ""},
	} {
		eng := sdkEngine(svc, claims)
		code, _ := do(t, eng, http.MethodGet, "/erp/sales/orders/"+other.ID)
		if code != http.StatusNotFound {
			t.Fatalf("dept_path=%q：GET 范围外的订单应答 404，实际 %d", claims.DeptPath, code)
		}
		if code, _ := do(t, eng, http.MethodGet, "/erp/sales/orders/999999999999"); code != http.StatusNotFound {
			t.Fatalf("GET 不存在的订单应答 404，实际 %d", code)
		}

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/erp/sales/orders/"+other.ID+"/confirm",
			bytes.NewBufferString(`{"idempotency_key":"`+uniq("rest-confirm-out")+`"}`))
		req.Header.Set("Content-Type", "application/json")
		eng.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("dept_path=%q：确认范围外的订单应答 403，实际 %d（%s）", claims.DeptPath, w.Code, w.Body.String())
		}
	}

	// 范围内照常 200：本部门的人、负责人本人。
	for _, claims := range []besdk.Claims{
		{Sub: uniq("u_manager"), DeptPath: "/1/"},
		{Sub: other.OwnerID, DeptPath: ""},
	} {
		if code, _ := do(t, sdkEngine(svc, claims), http.MethodGet, "/erp/sales/orders/"+other.ID); code != http.StatusOK {
			t.Fatalf("dept_path=%q sub=%s：GET 范围内的订单应答 200，实际 %d", claims.DeptPath, claims.Sub, code)
		}
	}
}
