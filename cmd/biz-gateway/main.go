// Package main 提供 Next Gateway 业务网关的薄进程入口。
package main

import (
	"github.com/nexthopecoming/next-gateway/internal/command"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

func main() {
	command.Main(role.Biz)
}
