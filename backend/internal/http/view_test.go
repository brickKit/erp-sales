package http

import (
	"net/http"
	"net/url"
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
)

// view 只有 dept / mine 两个取值（契约里是 enum）。拼错的 view 不能悄悄当成
// dept：调用者以为自己在看"我的订单"，拿到的却是整个部门的数。
func TestListOrders_REST不认识的view返回400(t *testing.T) {
	r, _ := testRepo(t)
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: "/1/"})
	for _, v := range []string{"Mine", "all", "owner"} {
		if code, _ := do(t, eng, http.MethodGet, "/erp/sales/orders?"+url.Values{"view": {v}}.Encode()); code != http.StatusBadRequest {
			t.Fatalf("GET /orders?view=%s 应返回 400，实际 %d", v, code)
		}
	}
	for _, v := range []string{"dept", "mine"} {
		if code, _ := do(t, eng, http.MethodGet, "/erp/sales/orders?"+url.Values{"view": {v}}.Encode()); code != http.StatusOK {
			t.Fatalf("GET /orders?view=%s 应返回 200，实际 %d", v, code)
		}
	}
}

func TestStatsSummary_REST不认识的view返回400(t *testing.T) {
	r, _ := testRepo(t)
	eng := newEngine(testService(r), besdk.Claims{Sub: "u_viewer", DeptPath: "/1/"})
	for _, v := range []string{"Mine", "all", "owner"} {
		if code, _ := getStats(t, eng, url.Values{"view": {v}}); code != http.StatusBadRequest {
			t.Fatalf("GET /stats/summary?view=%s 应返回 400，实际 %d", v, code)
		}
	}
	for _, v := range []string{"dept", "mine"} {
		if code, _ := getStats(t, eng, url.Values{"view": {v}}); code != http.StatusOK {
			t.Fatalf("GET /stats/summary?view=%s 应返回 200，实际 %d", v, code)
		}
	}
}

// 无部门的人经 REST 的 dept 视图：200 + 空列表 / 零，不是 4xx / 5xx。
func TestREST_无部门的人dept视图是空列表与零(t *testing.T) {
	r, _ := testRepo(t)
	me := uniq("u_nodept")
	createOrder(t, r, "/1/12/", uniq("u_other"))
	createOrder(t, r, "", uniq("u_other"))
	eng := newEngine(testService(r), besdk.Claims{Sub: me, DeptPath: ""})

	code, ids := listIDs(t, eng, url.Values{})
	if code != http.StatusOK || len(ids) != 0 {
		t.Fatalf("无部门的人 GET /orders 应是 200 + 空列表，实际 code=%d、%d 张", code, len(ids))
	}
	code, st := getStats(t, eng, url.Values{"period": {"week"}})
	if code != http.StatusOK || st.OrderCount != 0 || st.TotalAmount != "0.00" {
		t.Fatalf("无部门的人 GET /stats/summary 应是 200 + 零，实际 code=%d %+v", code, st)
	}
}
