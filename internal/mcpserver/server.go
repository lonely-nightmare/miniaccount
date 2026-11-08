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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/domain/voucher"
	"miniaccount/internal/service"
	"miniaccount/internal/store/sqlite"
)

// Options 是一次 MCP 服务实例的选项。
type Options struct {
	// ReadOnly 为真时只注册读工具（写工具不在工具表里，
	// 调用即协议层 tool-not-found）。
	ReadOnly bool
	// Operator 是审计里的操作人署名；空走服务层兜底。
	Operator string
	// Version 写进 initialize 响应的 serverInfo。
	Version string
}

// bookSource 抽象「这次调用落在哪本账上」：
// stdio 进程直挂一个固定 service；HTTP 经 BookProvider 在每次
// 调用上持读锁 + 复核账套世代（§2.3）。
type bookSource interface {
	call(ctx context.Context, fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error)
}

// directSource 是 stdio 用的直通实现。
type directSource struct{ svc *service.Service }

func (d directSource) call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	return fn(ctx, d.svc)
}

// Server 是一个绑定了账套的 MCP 服务。
type Server struct {
	src      bookSource
	readonly bool
	operator string
	core     *mcp.Server
	// OnChange 在写工具成功后回调（HTTP 同进程用来发
	// mcp:changed 通知前端刷新；stdio 为 nil）。
	OnChange func(entity string, id int64)
}

// New 构造服务并注册全部工具与资源。
func New(svc *service.Service, opts Options) *Server {
	return NewWithSource(directSource{svc: svc}, opts)
}

// NewWithSource 以自定义 bookSource 构造（HTTP BookProvider 用）。
func NewWithSource(src bookSource, opts Options) *Server {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{src: src, readonly: opts.ReadOnly, operator: opts.Operator}
	s.core = mcp.NewServer(&mcp.Implementation{
		Name:    "miniaccount",
		Version: version,
	}, nil)
	s.registerTools()
	s.registerResources()
	return s
}

// run 执行一次业务调用：补调用方身份 → 经 bookSource 持锁 →
// 业务错误统一走 toolError 映射，成功输出直通 StructuredContent。
// sessionedReq 抽象两类带 MCP 会话的请求（工具调用/资源读取），
// run 只关心它们的 GetSession()。
type sessionedReq interface{ GetSession() mcp.Session }

// sessionIDKey 是会话 ID 的调用上下文键。SDK 在会话 goroutine
// 派发 handler，HTTP 请求 ctx 的键值根本到不了这里；但 handler
// 的 req.Session.ID() 可用 —— 把它带进调用链，让 providerSource
// 在账套读锁内回查中间件钉住的世代。
type sessionIDKey struct{}

func withSessionID(ctx context.Context, req sessionedReq) context.Context {
	if req == nil {
		return ctx
	}
	if sess := req.GetSession(); sess != nil {
		if sid := sess.ID(); sid != "" {
			return context.WithValue(ctx, sessionIDKey{}, sid)
		}
	}
	return ctx
}

func (s *Server) run(ctx context.Context, req sessionedReq,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (*mcp.CallToolResult, any, error) {
	// 读工具没走 callCtx（不需要警告 sink），但路径上的
	// recordView/recordAudit 一样要写日志 —— 没 caller 会被
	// 记成「界面 + 操作系统用户名」，MCP 的查看/查询全被
	// 错记成 GUI 操作。写工具已带 sink 的 caller 不能覆盖。
	if !service.HasCaller(ctx) {
		ctx = service.WithCaller(ctx, service.Caller{
			Operator: s.operator, Source: audit.SourceMCP})
	}
	out, err := s.src.call(withSessionID(ctx, req), fn)
	if err != nil {
		return toolError(err)
	}
	return nil, out, nil
}

// changed 在写工具成功后通知前端（HTTP 同进程 → mcp:changed；
// stdio 跨进程通知不到，OnChange 为 nil 时静默跳过）。
func (s *Server) changed(entity string, id int64) {
	if s.OnChange != nil {
		s.OnChange(entity, id)
	}
}

// recordReportView 记一次「查看报表」（§2.6：query 走 RecordView，
// 与 GUI 报表查看同一条审计口径；查看开关关着时它什么都不做）。
func (s *Server) recordReportView(ctx context.Context, svc *service.Service,
	kind, period string) {
	svc.RecordView(ctx, service.AuditEvent{
		Action: audit.ActionViewReport, Entity: "report",
		EntityID: kind + "/" + period,
		Summary:  "查看报表：" + kind,
		Detail:   map[string]any{"报表": kind, "期间": period},
	})
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
	case errors.As(err, &inv), errors.Is(err, voucher.ErrBadWord):
		code = "invalid_params"
	case errors.Is(err, service.ErrIdempotencyConflict):
		code = "idempotency_conflict"
	case errors.Is(err, service.ErrIdempotentTargetDeleted):
		code = "idempotent_target_deleted"
	case errors.Is(err, sqlite.ErrVoucherConflict):
		code = "conflict"
	case errors.Is(err, sqlite.ErrNotFound), errors.Is(err, service.ErrNoBook):
		code = "not_found"
	case errors.Is(err, errStaleSession):
		code = "session_stale"
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
// ★ 必须 UseNumber：普通解出的 float64 会吃掉 >2^53 的 id
// （9007199254740993 → ...992），把这条数据再 marshal 出去，
// 客户端照抄 id 就改到另一条档案上。json.Number 保字面量，
// 重新序列化原样输出。
func mustJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
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
			if n, ok := val.(json.Number); ok && moneyKeys[strings.ToLower(k)] {
				if cents, err := n.Int64(); err == nil {
					t[k] = yuanStr(money.Money(cents))
					continue
				}
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
