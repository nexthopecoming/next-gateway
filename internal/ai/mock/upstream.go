// Package mock 提供不访问外部网络的确定性 AI 上游。
package mock

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nexthopecoming/next-gateway/internal/core/config"
	"github.com/nexthopecoming/next-gateway/internal/core/engine"
)

// Upstream 使用启动时注入的不可变 Mock 配置，可安全并发执行请求。
type Upstream struct {
	config config.MockConfig
}

// New 创建共享 Mock 上游，stable/canary 和两种协议均复用同一实例。
func New(configuration config.MockConfig) *Upstream {
	return &Upstream{config: configuration}
}

// Complete 复用统一生成计划，不套用流式 chunk 间延迟。
func (u *Upstream) Complete(ctx context.Context, request engine.Request) (engine.Response, error) {
	if err := ctx.Err(); err != nil {
		return engine.Response{}, err
	}
	return u.response(request), nil
}

// Stream 只启动一个可取消 goroutine；等待和发送都 select ctx，慢消费者退出后不会滞留。
func (u *Upstream) Stream(ctx context.Context, request engine.Request) (<-chan engine.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response := u.response(request)
	chunks := splitRunes(response.Text, u.config.ChunkSize)
	events := make(chan engine.Event)
	go func() {
		defer close(events)
		for index, chunk := range chunks {
			// 第一段立即发送，延迟只存在于相邻内容块之间。
			if index > 0 && u.config.ChunkDelay > 0 {
				timer := time.NewTimer(u.config.ChunkDelay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
			}
			select {
			case events <- engine.Event{Kind: engine.EventContent, Content: chunk}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case events <- engine.Event{Kind: engine.EventDone, Response: response}:
		case <-ctx.Done():
		}
	}()
	return events, nil
}

// response 是 JSON 与 SSE 共用的唯一文本、结束原因和 usage 计算入口。
func (u *Upstream) response(request engine.Request) engine.Response {
	output := []rune(u.config.ResponseText)
	end := len(output)
	finishReason := engine.FinishStop
	if request.MaxTokens > 0 && end > request.MaxTokens {
		end = request.MaxTokens
		finishReason = engine.FinishLength
	}

	stopIndex, stopSequence := earliestStop(u.config.ResponseText, request.Stop)
	// max_tokens 已先截断时，其后的 stop 不可见；否则采用最早命中的 stop。
	if stopIndex >= 0 && (stopIndex < end || finishReason != engine.FinishLength && stopIndex == end) {
		end = stopIndex
		finishReason = engine.FinishStopSequence
	} else {
		stopSequence = ""
	}

	inputTokens := utf8.RuneCountInString(request.System)
	for _, message := range request.Messages {
		inputTokens += utf8.RuneCountInString(message.Content)
	}
	return engine.Response{
		Text:         string(output[:end]),
		FinishReason: finishReason,
		StopSequence: stopSequence,
		Usage: engine.Usage{
			InputTokens:  inputTokens,
			OutputTokens: end,
		},
	}
}

// earliestStop 返回按 code point 计算的最早命中位置；同位置按请求中的 stop 顺序选择。
func earliestStop(text string, stops []string) (int, string) {
	best := -1
	matched := ""
	for _, stop := range stops {
		if stop == "" {
			continue
		}
		byteIndex := strings.Index(text, stop)
		if byteIndex < 0 {
			continue
		}
		index := utf8.RuneCountInString(text[:byteIndex])
		if best < 0 || index < best {
			best = index
			matched = stop
		}
	}
	return best, matched
}

func splitRunes(text string, size int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, 1+(len(runes)-1)/size)
	for start := 0; start < len(runes); start += size {
		end := min(start+size, len(runes))
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}
