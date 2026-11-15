// Package model 提供入站协议与静态路由共用的模型键约束。
// 本包不读取请求正文，也不保存或记录模型原文。
package model

import "strings"

const (
	// MaxBytes 限制模型键的 UTF-8 原始字节数，而不是 Unicode code point 数。
	MaxBytes = 128
)

// Valid 只校验非空白和字节上限；调用方仍按原始值进行大小写敏感匹配。
func Valid(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= MaxBytes
}
