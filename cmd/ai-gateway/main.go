// Package main 提供 Next Gateway AI 数据面网关的薄进程入口。
package main

import (
	"github.com/nexthopecoming/next-gateway/internal/command"
	"github.com/nexthopecoming/next-gateway/internal/core/role"
)

func main() {
	command.Main(role.AI)
}
