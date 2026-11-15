// Package httpserver 提供业务 HTTP 中间件、健康检查和优雅退出。
package httpserver

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"
)

// NewHandler 建立健康路由并允许 app 注入领域路由，core 因此无需 import biz/ai。
// 外层请求 ID 保证恢复和访问日志使用同一标识。
func NewHandler(ready *atomic.Bool, logger *zap.Logger, registrars ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
	})
	for _, register := range registrars {
		if register != nil {
			register(mux)
		}
	}

	handler := http.Handler(mux)
	handler = withRecovery(logger)(handler)
	handler = withAccessLog(logger)(handler)
	handler = withRequestID(logger)(handler)
	return handler
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
