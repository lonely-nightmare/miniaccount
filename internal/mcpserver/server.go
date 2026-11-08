// Package mcpserver 把账套能力以 MCP 工具/资源的形式暴露给外部模型与自动化。
//
// 设计见 docs/删除AI模块改为MCP服务能力.md：
//   - 14 个只读工具 + 6 个写工具 + 3 个资源，无 prompts；
//   - 金额一律以元字符串出入（内部 money.Money 分），不暴露整数分；
//   - 写路径全部过 service 层校验与审计，不绕开会计红线；
//   - 显式不暴露：过账、结账/反结账、备份恢复、附件、审计读写、
//     密钥配置、任意 SQL、文件系统。
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/service"
	"miniaccount/internal/store/sqlite"
)

// Options 是一次 MCP 服务实例的选项。
type Options struct {
	// ReadOnly 为真时写工具仍注册但一律返回 readonly 错误
	// （客户端看得见工具形状，只是不能用）。
	ReadOnly bool
	// Operator 是审计里的操作人署名；空走服务层兜底。
	Operator string
	// Version 写进 initialize 响应的 serverInfo。
	Version string
}

// Server 是一个绑定了账套的 MCP 服务。
type Server struct {
	svc      *service.Service
	readonly bool
	operator string
	core     *mcp.Server
}

// New 构造服务并注册全部工具与资源。
func New(svc *service.Service, opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{svc: svc, readonly: opts.ReadOnly, operator: opts.Operator}
	s.core = mcp.NewServer(&mcp.Implementation{
		Name:    "miniaccount",
		Version: version,
	}, nil)
	s.registerTools()
	s.registerResources()
	return s
}

// Run 在给定 Transport 上服务，直到对端断开或 ctx 结束。
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	return s.core.Run(ctx, t)
}

// RunStdio 在标准输入输出上服务（cmd 子命令的入口）。
func (s *Server) RunStdio(ctx context.Context) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}

// callCtx 把「本次调用来自 MCP、操作人是谁」注入 ctx，
// 并挂一个警告收集器 —— 写工具的响应里带 auditWarnings。
func (s *Server) callCtx(ctx context.Context) (context.Context, *service.CallWarnings) {
	w := &service.CallWarnings{}
	return service.WithCaller(ctx, service.Caller{
		Operator: s.operator,
		Source:   audit.SourceMCP,
		Warn:     w,
	}), w
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// toolError 把业务错误翻译成工具结果里的错误：
// 协议层 error 留给「工具不存在/参数解不动」这类调用问题，
// 业务失败走 IsError + 错误名前缀，模型才能把
// idempotency_conflict / conflict 当成可重试的语义。
func toolError(err error) (*mcp.CallToolResult, any, error) {
	code := "internal"
	var inv *service.InvalidInput
	switch {
	case errors.As(err, &inv):
		code = "invalid_params"
	case errors.Is(err, service.ErrIdempotencyConflict):
		code = "idempotency_conflict"
	case errors.Is(err, service.ErrIdempotentTargetDeleted):
		code = "idempotent_target_deleted"
	case errors.Is(err, sqlite.ErrVoucherConflict):
		code = "conflict"
	case errors.Is(err, sqlite.ErrNotFound), errors.Is(err, service.ErrNoBook):
		code = "not_found"
	case errors.Is(err, sqlite.ErrAlreadyClosed),
		errors.Is(err, sqlite.ErrNotClosed),
		errors.Is(err, sqlite.ErrPriorOpen),
		errors.Is(err, sqlite.ErrLaterClosed),
		errors.Is(err, sqlite.ErrClosedVoucher),
		errors.Is(err, period.ErrNotOpen),
		errors.Is(err, period.ErrPeriodNotFound):
		code = "blocked"
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: code + ": " + err.Error()},
		},
	}, nil, nil
}

// readonlyErr 是 --readonly 模式下写工具的回答。
func readonlyErr() (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{
			&mcp.TextContent{Text: "readonly: 服务以只读模式启动，写操作不可用"},
		},
	}, nil, nil
}

// ---------------------------------------------------------------------------
// 值转换
// ---------------------------------------------------------------------------

// parseYuan 把元字符串解析成分。
func parseYuan(s string) (money.Money, error) {
	m, err := money.Parse(s)
	if err != nil {
		return 0, fmt.Errorf("金额 %q 解析失败", s)
	}
	return m, nil
}

// yuanStr 把分格式化为元字符串。
func yuanStr(m money.Money) string { return m.String() }

// mustJSON 把任意值转成 map/slice —— 工具输出统一走
// StructuredContent，JSON 往返是拿现成结构的最短路径。
func mustJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return out
}
