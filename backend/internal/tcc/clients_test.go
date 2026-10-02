package tcc

import (
	"os"
	"strings"

	besdk "github.com/brickKit/be-sdk-go"

	"github.com/brickKit/erp-sales/v2/backend/internal/client"
)

// testClients 用测试进程的环境构造依赖地址配置：测试进程不是模块，
// test-cross.sh 把依赖地址（<ID>_GRPC_ENDPOINT）注入成环境变量。
func testClients() *client.Clients {
	vals := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	return client.New(besdk.NewConfig(vals))
}
