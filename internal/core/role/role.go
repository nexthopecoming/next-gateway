// Package role 定义网关进程职责的固定枚举，供命令入口、应用装配和后续按角色校验共享。
package role

// Role 标识进程职责；它只影响入口元数据、默认 profile、固定 service 日志和后续按角色约束，
// 不代表 stable/canary 执行器。
type Role string

const (
	// Biz 是业务网关进程，后续承载业务入口和管理面。
	Biz Role = "biz-gateway"
	// AI 是 AI 数据面网关进程，后续承载双协议与 SSE。
	AI Role = "ai-gateway"
)

// String 返回用于 CLI 元数据、默认 profile 和日志字段的固定标识。
func (r Role) String() string {
	return string(r)
}
