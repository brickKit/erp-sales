package module

import (
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
)

// 模块按 configSchema 里的确切键名读配置；缺了必填项时 New 返回 error，
// 不 panic——进了外壳，一个成员 panic 会让同一外壳里的成员全跟着退出。

func TestReadConfig_按configSchema的键名读(t *testing.T) {
	got, err := readConfig(besdk.NewConfig(map[string]string{
		"DEFAULT_WAREHOUSE_ID": "7", "EXCEPTION_ASSIGNEE_SUB": "u_ops_reviewer", "PG_SCHEMA": "erp_sales_x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := settings{schema: "erp_sales_x", role: "erp_sales_x_rw", defaultWarehouseID: "7", exceptionAssigneeSub: "u_ops_reviewer"}
	if got != want {
		t.Fatalf("期望 %+v，实际 %+v", want, got)
	}

	got, err = readConfig(besdk.NewConfig(map[string]string{"DEFAULT_WAREHOUSE_ID": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.schema != "erp_sales" || got.role != "erp_sales_rw" || got.exceptionAssigneeSub != "" {
		t.Fatalf("没配的键应取默认值（schema erp_sales、待办不指派），实际 %+v", got)
	}

	// 驼峰旧写法不是这些键
	got, err = readConfig(besdk.NewConfig(map[string]string{"DEFAULT_WAREHOUSE_ID": "1", "exceptionAssigneeSub": "u_old"}))
	if err != nil || got.exceptionAssigneeSub != "" {
		t.Fatalf("exceptionAssigneeSub 不是 EXCEPTION_ASSIGNEE_SUB，不该被读到，实际 %+v（%v）", got, err)
	}
}

func TestReadConfig_缺默认仓库时返回错误而不是panic(t *testing.T) {
	for _, vals := range []map[string]string{{}, {"DEFAULT_WAREHOUSE_ID": ""}, {"defaultWarehouseId": "1"}} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%v：缺 DEFAULT_WAREHOUSE_ID 应返回 error，实际 panic：%v", vals, p)
				}
			}()
			if _, err := readConfig(besdk.NewConfig(vals)); err == nil {
				t.Errorf("%v：缺 DEFAULT_WAREHOUSE_ID 应返回 error", vals)
			}
		}()
	}
}
