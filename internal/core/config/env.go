package config

import "strings"

// newEnvKeyReplacer 单独构造函数，便于测试确认 server.listen 映射为 SERVER_LISTEN。
func newEnvKeyReplacer() *strings.Replacer {
	return strings.NewReplacer(".", "_", "-", "_")
}
