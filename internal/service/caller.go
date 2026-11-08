// 调用者上下文：外部通道（MCP 等）经 WithCaller 把
// 「这次调用是谁、来自哪里」带进 service，审计日志照实记。
//
// 见 docs/删除AI模块改为MCP服务能力.md §2.6。
package service

import (
	"context"
	"sync"

	"miniaccount/internal/domain/audit"
)

// CallWarnings 收集一次调用里「不致命但调用方必须知道」的警告。
//
// 主要用途是审计写失败：MCP 适配层调完业务方法后读 All()，
// 映射为工具结果的 auditWarning 字段 —— 让外部知道「操作成了，
// 但日志没记上」。GUI 不传 sink 时行为同今（stderr）。
type CallWarnings struct {
	mu   sync.Mutex
	list []string
}

// Add 追加一条警告。
func (w *CallWarnings) Add(msg string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.list = append(w.list, msg)
	w.mu.Unlock()
}

// All 返回已收集的警告（可能为空）。
func (w *CallWarnings) All() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.list))
	copy(out, w.list)
	return out
}

// Caller 是一次业务调用的调用方身份。
type Caller struct {
	// Operator 是操作人署名（审计 operator 字段）。留空走现有兜底。
	Operator string
	// Source 是调用来源（审计 source 字段）。留空走现有兜底。
	Source audit.Source
	// Warn 收集本次调用的非致命警告（如审计写失败）。可为 nil。
	Warn *CallWarnings
}

type callerKey struct{}

// WithCaller 返回带调用方身份的 ctx。
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// callerOf 取调用方身份；没带时返回零值 Caller。
func callerOf(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Caller{}
}
