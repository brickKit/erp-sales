package main

import (
	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-sales/v2/backend/module"
)

// 这个文件永远只有这一行。装配的全部逻辑（OTel、连接池、NATS、Listen HTTP
// 与 gRPC、信号处理、优雅关停）都在 besdk.RunStandalone 里，它也是唯一读进程
// 环境变量的地方。进外壳时，外壳调的是同一个 module.New——合并只发生在部署
// 形态上，代码不分两套。
func main() { besdk.RunStandalone(module.New) }
