//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestGatewayProcesses 使用真实二进制验证两个角色可以同时运行并响应同一退出契约。
// 动态端口避免测试与开发者正在运行的 8080/8081 服务互相干扰。
func TestGatewayProcesses(t *testing.T) {
	tests := []struct {
		name   string
		signal os.Signal
	}{
		{name: "SIGINT", signal: os.Interrupt},
		{name: "SIGTERM", signal: syscall.SIGTERM},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testGatewayProcesses(t, test.signal)
		})
	}
}

// testGatewayProcesses 对每种退出信号重复真实双进程检查，避免只验证 SIGTERM。
func testGatewayProcesses(t *testing.T, shutdownSignal os.Signal) {
	root := repositoryRoot(t)
	tempDir := t.TempDir()
	processes := make([]*gatewayProcess, 0, 2)
	for _, role := range []string{"biz-gateway", "ai-gateway"} {
		binary := filepath.Join(tempDir, role)
		buildGateway(t, root, role, binary)
		processes = append(processes, startGateway(t, root, role, binary, availableAddress(t)))
	}

	client := &http.Client{Timeout: time.Second}
	for _, process := range processes {
		waitUntilReady(t, client, process)
		for _, endpoint := range []struct {
			path       string
			wantStatus string
		}{
			{path: "/healthz", wantStatus: "ok"},
			{path: "/readyz", wantStatus: "ready"},
		} {
			assertEndpoint(t, client, process.address, endpoint.path, endpoint.wantStatus)
		}
	}

	// 同时发出退出信号，避免一个进程的等待掩盖另一个进程的退出问题。
	for _, process := range processes {
		if err := process.command.Process.Signal(shutdownSignal); err != nil {
			t.Fatalf("向 %s 发送 %s: %v", process.role, shutdownSignal, err)
		}
	}
	for _, process := range processes {
		if err := process.waitForExit(10 * time.Second); err != nil {
			t.Fatalf("%s 优雅退出失败: %v\n%s", process.role, err, process.logContents())
		}
		logs := process.logContents()
		if !strings.Contains(logs, `"service":"`+process.role+`"`) {
			t.Errorf("%s 日志缺少固定 service 字段: %s", process.role, logs)
		}
		if !strings.Contains(logs, `"version":"dev"`) {
			t.Errorf("%s 启动日志缺少模块版本: %s", process.role, logs)
		}
		if !strings.Contains(logs, "HTTP 服务已优雅停止") {
			t.Errorf("%s 日志缺少优雅停止记录: %s", process.role, logs)
		}
	}
}

// TestGatewayAIProtocols 使用两个相同配置的 AI 进程验证协议、跨实例灰度和 biz 路由隔离。
func TestGatewayAIProtocols(t *testing.T) {
	root := repositoryRoot(t)
	tempDir := t.TempDir()
	bizBinary := filepath.Join(tempDir, "biz-gateway")
	aiBinary := filepath.Join(tempDir, "ai-gateway")
	buildGateway(t, root, "biz-gateway", bizBinary)
	buildGateway(t, root, "ai-gateway", aiBinary)

	biz := startGateway(t, root, "biz-gateway", bizBinary, availableAddress(t))
	canaryEnvironment := []string{"NEXT_GATEWAY_CANARY_CANARY_BASIS_POINTS=2500"}
	aiOne := startGatewayWithEnvironment(t, root, "ai-gateway-1", aiBinary, availableAddress(t), canaryEnvironment)
	aiTwo := startGatewayWithEnvironment(t, root, "ai-gateway-2", aiBinary, availableAddress(t), canaryEnvironment)
	processes := []*gatewayProcess{biz, aiOne, aiTwo}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, process := range processes {
		waitUntilReady(t, client, process)
	}

	// tenant-001/Tenant-001 的黄金 bucket 分别为 2023/3351，在 2500 阈值下覆盖两条执行链。
	for _, test := range []struct {
		tenant   string
		wantCore string
	}{
		{tenant: "tenant-001", wantCore: "canary"},
		{tenant: "Tenant-001", wantCore: "stable"},
	} {
		for _, process := range []*gatewayProcess{aiOne, aiTwo} {
			response := postGateway(t, client, process.address, "/v1/chat/completions",
				`{"model":"process-model","messages":[{"role":"user","content":"process secret prompt"}]}`,
				map[string]string{
					"X-Request-ID":  "req_process_openai",
					"X-Tenant-ID":   test.tenant,
					"Authorization": "Bearer sk-process-secret",
				})
			if response.status != http.StatusOK || response.header.Get("X-Gateway-Core") != test.wantCore {
				t.Fatalf("%s 租户 %s 灰度错误: status=%d core=%q body=%s", process.role, test.tenant, response.status, response.header.Get("X-Gateway-Core"), response.body)
			}
			if response.header.Get("X-Request-ID") != "req_process_openai" || !strings.Contains(response.body, `"content":"Hello from Next Gateway Mock"`) {
				t.Fatalf("%s OpenAI 响应错误: headers=%v body=%s", process.role, response.header, response.body)
			}
		}
	}

	stream := postGateway(t, client, aiOne.address, "/v1/messages",
		`{"model":"process-model","max_tokens":100,"messages":[{"role":"user","content":"anthropic secret prompt"}],"stream":true}`,
		map[string]string{
			"X-Request-ID":      "req_process_anthropic",
			"anthropic-version": "2023-06-01",
			"x-api-key":         "sk-anthropic-process-secret",
		})
	if stream.status != http.StatusOK || !strings.HasPrefix(stream.header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Anthropic SSE 响应头错误: status=%d headers=%v body=%s", stream.status, stream.header, stream.body)
	}
	for _, expected := range []string{"event: message_start\n", "event: content_block_delta\n", `"text":"Hello fr"`, `"text":"Mock"`, "event: message_stop\n"} {
		if !strings.Contains(stream.body, expected) {
			t.Errorf("Anthropic SSE 缺少 %q: %s", expected, stream.body)
		}
	}
	if core := stream.header.Get("X-Gateway-Core"); core != "stable" && core != "canary" {
		t.Errorf("Anthropic X-Gateway-Core = %q", core)
	}

	// biz-gateway 只提供共享健康端点，不注册或转发两条 AI 协议路由。
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		response := postGateway(t, client, biz.address, path, `{}`, nil)
		if response.status != http.StatusNotFound || response.header.Get("X-Gateway-Core") != "" {
			t.Errorf("biz AI 路由 %s = %d core=%q", path, response.status, response.header.Get("X-Gateway-Core"))
		}
	}

	for _, process := range processes {
		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("停止 %s: %v", process.role, err)
		}
	}
	for _, process := range processes {
		if err := process.waitForExit(10 * time.Second); err != nil {
			t.Fatalf("%s 退出失败: %v\n%s", process.role, err, process.logContents())
		}
		logs := process.logContents()
		for _, forbidden := range []string{
			"tenant-001", "Tenant-001", "sk-process-secret", "sk-anthropic-process-secret",
			"process-model", "process secret prompt", "anthropic secret prompt", "Hello from Next Gateway Mock",
		} {
			if strings.Contains(logs, forbidden) {
				t.Errorf("%s 日志包含敏感或无界内容 %q: %s", process.role, forbidden, logs)
			}
		}
	}
}

type gatewayHTTPResponse struct {
	status int
	header http.Header
	body   string
}

func postGateway(t *testing.T, client *http.Client, address, path, body string, headers map[string]string) gatewayHTTPResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+address+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatalf("读取 %s 响应: %v", path, err)
	}
	return gatewayHTTPResponse{status: response.StatusCode, header: response.Header.Clone(), body: string(content)}
}

type gatewayProcess struct {
	role     string
	address  string
	command  *exec.Cmd
	logFile  *os.File
	logPath  string
	waitDone chan struct{}
	waitErr  error
}

func buildGateway(t *testing.T, root, role, output string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 不通过 go run 启动进程，否则信号可能只到达 go 工具而非真实网关。
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, "./cmd/"+role)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("构建 %s: %v\n%s", role, err, output)
	}
}

func startGateway(t *testing.T, root, role, binary, address string) *gatewayProcess {
	t.Helper()
	return startGatewayWithEnvironment(t, root, role, binary, address, nil)
}

// startGatewayWithEnvironment 只显式注入测试所需配置，清除外部环境避免灰度结果受开发机影响。
func startGatewayWithEnvironment(t *testing.T, root, role, binary, address string, extraEnvironment []string) *gatewayProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), role+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "serve", "--listen", address)
	command.Dir = root
	command.Env = append(environmentWithoutGatewayOverrides(), extraEnvironment...)
	command.Stdout = logFile
	command.Stderr = logFile
	process := &gatewayProcess{
		role:     role,
		address:  address,
		command:  command,
		logFile:  logFile,
		logPath:  logPath,
		waitDone: make(chan struct{}),
	}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("启动 %s: %v", role, err)
	}
	go func() {
		process.waitErr = command.Wait()
		close(process.waitDone)
	}()
	t.Cleanup(func() {
		select {
		case <-process.waitDone:
		default:
			_ = command.Process.Kill()
			select {
			case <-process.waitDone:
			case <-time.After(5 * time.Second):
				t.Errorf("清理 %s 子进程超时", role)
			}
		}
		if err := logFile.Close(); err != nil {
			t.Errorf("关闭 %s 日志: %v", role, err)
		}
	})
	return process
}

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func environmentWithoutGatewayOverrides() []string {
	environment := os.Environ()
	filtered := environment[:0]
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "NEXT_GATEWAY_") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func waitUntilReady(t *testing.T, client *http.Client, process *gatewayProcess) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	url := "http://" + process.address + "/readyz"
	for {
		response, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-process.waitDone:
			t.Fatalf("%s 在就绪前退出: %v\n%s", process.role, process.waitErr, process.logContents())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 就绪超时\n%s", process.role, process.logContents())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertEndpoint(t *testing.T, client *http.Client, address, path, wantStatus string) {
	t.Helper()
	response, err := client.Get("http://" + address + path)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s 状态码 = %d", path, response.StatusCode)
	}
	if response.Header.Get("X-Request-ID") == "" {
		t.Errorf("%s 缺少 X-Request-ID", path)
	}
	var body map[string]string
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body); err != nil {
		t.Fatalf("解析 %s 响应: %v", path, err)
	}
	if body["status"] != wantStatus {
		t.Errorf("%s status = %q，期望 %q", path, body["status"], wantStatus)
	}
}

func (p *gatewayProcess) waitForExit(timeout time.Duration) error {
	select {
	case <-p.waitDone:
		return p.waitErr
	case <-time.After(timeout):
		_ = p.command.Process.Kill()
		<-p.waitDone
		return fmt.Errorf("等待退出超过 %s", timeout)
	}
}

func (p *gatewayProcess) logContents() string {
	_ = p.logFile.Sync()
	content, err := os.ReadFile(p.logPath)
	if err != nil {
		return "读取日志失败: " + err.Error()
	}
	return string(content)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
