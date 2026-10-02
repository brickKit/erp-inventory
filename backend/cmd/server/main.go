package main

import (
	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/erp-inventory/v2/backend/module"
)

// ⚠️ 这个文件永远只有这一行：单跑与进外壳走同一个 module.New，装配逻辑不许写在这里。
func main() { besdk.RunStandalone(module.New) }
