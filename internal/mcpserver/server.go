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
	"strings"

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

// moneyKeys 是所有工具输出里「数字其实是分的金额」的 JSON 键名
// （小写比较；报表类型没有 json tag，序列化出来是 PascalCase，
// 所以两侧都按小写比对）。
//
// ★ 集合刻意收窄到 6 个涉金额工具的真实输出键：
// total/count/level/limit/offset/taxrateppm/confidence 这些
// 非金额数字键绝不能进集 —— 分页总数被改成元字符串就是新事故。
var moneyKeys = map[string]bool{
	// 凭证/流水/明细账
	"amount": true, "debit": true, "credit": true, "balance": true,
	"openingbalance": true, "closingbalance": true,
	// 试算平衡 BalanceRow（无 tag → PascalCase 序列化名）
	"openingdebit": true, "openingcredit": true,
	"perioddebit": true, "periodcredit": true,
	"closingdebit": true, "closingcredit": true,
	// 报表 Line.Value 与勾稽 CheckIssue
	"value": true, "leftval": true, "rightval": true, "diff": true,
	// 发票
	"amountextax": true, "taxamount": true, "totalamount": true,
	"deductible": true, "deductibletax": true, "costamount": true,
	// 员工/工资
	"basesalary": true, "sibase": true, "hfbbase": true,
	"specialadditional": true,
	"totalgross":        true, "totaliit": true, "totalsiself": true,
	"totalsicompany": true, "totaldebit": true, "totalcredit": true,
	"totalin": true, "totalout": true,
}

// yuanize 把 mustJSON 输出里所有「键名属金额、值是数字」的字段
// 改写成元字符串 —— MCP 契约：金额一律元字符串出入，不暴露分。
// 字符串/布尔等同名键不受影响（只有数字值才被改写）。
func yuanize(v any) any {
	return yuanWalk(mustJSON(v))
}

func yuanWalk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if f, ok := val.(float64); ok && moneyKeys[strings.ToLower(k)] {
				t[k] = yuanStr(money.Money(int64(f)))
				continue
			}
			t[k] = yuanWalk(val)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = yuanWalk(e)
		}
		return t
	default:
		return v
	}
}

// pageIn 是列表工具的分页参数；适配层对完整结果集切片
// （档案/流水规模小，服务端本就全量物化，无需另加 COUNT 查询）。
type pageIn struct {
	Limit  int `json:"limit,omitempty" jsonschema:"每页行数，上限 500，缺省 500"`
	Offset int `json:"offset,omitempty" jsonschema:"跳过的行数"`
}

// pageOf 把完整列表包成 {items,total,limit,offset,hasMore}。
func pageOf(v any, in pageIn) any {
	arr, _ := mustJSON(v).([]any)
	total := len(arr)
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return map[string]any{
		"items": arr[offset:end], "total": total,
		"limit": limit, "offset": offset, "hasMore": end < total,
	}
}
