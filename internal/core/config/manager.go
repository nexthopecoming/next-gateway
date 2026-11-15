package config

import "sync"

// Manager 是启动期一次性配置加载器，不负责 Watch、热更新或运行时快照替换。
type Manager struct {
	once sync.Once
	cfg  Config
	err  error
}

var (
	defaultManagerOnce sync.Once
	defaultManager     *Manager
)

// NewManager 返回独立的启动加载器，供隔离装配和测试使用。
func NewManager() *Manager {
	return &Manager{}
}

// DefaultManager 返回进程唯一的启动 Manager；仅在首次调用时创建实例，不提前读取配置。
func DefaultManager() *Manager {
	defaultManagerOnce.Do(func() {
		defaultManager = NewManager()
	})
	return defaultManager
}

// Load 只采用第一次调用的选项，并缓存首次配置或错误；后续选项不会触发重新加载。
// 成功时每次返回独立副本，失败时返回零值配置和首次缓存的错误。
func (m *Manager) Load(opts ...Option) (Config, error) {
	m.once.Do(func() {
		m.cfg, m.err = Load(opts...)
	})
	if m.err != nil {
		return Config{}, m.err
	}
	return m.cfg.clone(), nil
}
