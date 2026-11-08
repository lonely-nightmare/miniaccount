package mcpserver

// ---------------------------------------------------------------------------
// HTTP 传输（设计文档 §2.2/§2.3）
//
// 四层访问控制，顺序固定为 Host → Origin → Token → 会话世代：
//   1. Host   —— 精确匹配 127.0.0.1:<port> / localhost:<port>，挡 DNS rebinding
//   2. Origin —— 有 Origin 头时只允许 localhost/127.0.0.1 来源；
//               无 Origin（curl、客户端库等非浏览器）放行
//   3. Token  —— Authorization: Bearer <token>，256-bit，存
//               ~/.mini-account/mcp.json（0600，不入账套）
//   4. 世代   —— MCP-Session-Id 在注册表里的 (bookID, generation)
//               与当前账套不符 → 404，客户端按规范重新 initialize
//
// 读锁只覆盖一次业务调用（BookProvider.Call），不覆盖整个 HTTP
// 连接；已建立的 SSE 流不持锁也不强杀 —— 本服务不主动向流上推
// 事件，过期的 GET 流是空转的，下一次请求在世代层被 404 后
// 客户端即按规范重建会话。
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/service"
)

// sessionHeader 是 MCP 规范里的会话头名。
const sessionHeader = "Mcp-Session-Id"

// BookProvider 是 HTTP 模式访问「当前账套」的唯一入口。
// desktop.App 实现它：Call 用 App.mu.RLock 覆盖一次业务调用；
// Generation 在 Open/Close/Restore 换账套时递增。
type BookProvider interface {
	// Call 在持读锁的临界区内执行一次业务调用。
	// 未打开账套时返回 service.ErrNoBook。
	Call(ctx context.Context, fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error)
	// BookID 返回当前账套标识（= 数据库路径）。
	BookID() string
	// Generation 返回当前账套世代；账套一切换就递增。
	Generation() int64
}

// providerSource 把 BookProvider 适配成 bookSource。
type providerSource struct{ p BookProvider }

func (p providerSource) call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	return p.p.Call(ctx, fn)
}

// ---------------------------------------------------------------------------
// 凭据：~/.mini-account/mcp.json（0600）
// ---------------------------------------------------------------------------

// MCPConfig 是 mcp.json 的内容。Token 不放进账套库 ——
// 账套可以被备份/复制/恢复，凭据绝不能跟着走。
type MCPConfig struct {
	Token    string `json:"token"`
	Port     int    `json:"port"`
	Operator string `json:"operator"`
}

// ConfigStore 负责 mcp.json 的原子读写与权限收紧。
type ConfigStore struct {
	Path string
}

// DefaultConfigStore 返回 ~/.mini-account/mcp.json 的存储。
func DefaultConfigStore() (*ConfigStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("找不到用户主目录: %w", err)
	}
	return &ConfigStore{Path: filepath.Join(home, ".mini-account", "mcp.json")}, nil
}

// Load 读取配置；文件不存在返回 (nil, nil)，坏文件直接报错
// （宁可起不来也不静默换一份凭据）。
func (s *ConfigStore) Load() (*MCPConfig, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %w", s.Path, err)
	}
	var c MCPConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s 不是合法 JSON（凭据可能损坏）: %w", s.Path, err)
	}
	// Token 为空是合法的「吊销态」（MCPStop 关闭服务即吊销），
	// 调用方据此决定要不要重新签发；只有解析不了的文件才报错。
	return &c, nil
}

// Save 原子写入并收紧权限为 0600：先写临时文件 → chmod → rename，
// 避免写一半留下坏 JSON，也避免权限窗口。
func (s *ConfigStore) Save(c *MCPConfig) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(s.Path, 0o600)
}

// GenerateToken 生成 256-bit 凭据（base64url 编码）。
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成随机凭据失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// tokenHolder 让「重新生成」立即生效 —— 校验读的是
// 这个原子格子里的值，不用重启服务。
type tokenHolder struct{ v atomicString }

type atomicString struct {
	mu sync.RWMutex
	s  string
}

func (h *tokenHolder) get() string { h.v.mu.RLock(); defer h.v.mu.RUnlock(); return h.v.s }
func (h *tokenHolder) set(s string) {
	h.v.mu.Lock()
	h.v.s = s
	h.v.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 会话世代注册表
// ---------------------------------------------------------------------------

// sessMeta 是一次 initialize 时钉下的账套快照。
type sessMeta struct {
	bookID     string
	generation int64
}

// sessionTable 记录 MCP-Session-Id → 钉住的账套世代。
// 账套切换（generation++）后旧表项天然失效；
// 表项在「下一次带该 ID 的请求」到达时惰性清除。
type sessionTable struct {
	mu sync.Mutex
	m  map[string]sessMeta
}

func newSessionTable() *sessionTable { return &sessionTable{m: make(map[string]sessMeta)} }

func (t *sessionTable) put(id string, meta sessMeta) {
	t.mu.Lock()
	t.m[id] = meta
	t.mu.Unlock()
}

// check 返回 true 表示会话与当前账套一致；不一致（或已注销）
// 的会话被移除并返回 false。
func (t *sessionTable) check(id string, p BookProvider) bool {
	t.mu.Lock()
	meta, ok := t.m[id]
	if !ok || meta.bookID != p.BookID() || meta.generation != p.Generation() {
		delete(t.m, id)
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()
	return true
}

// bufferWriter 完整缓冲 initialize 响应：
//  1. 从响应头取签发的 MCP-Session-Id 钉入世代注册表；
//  2. 往 JSON-RPC result 里注入 _meta{bookId, generation}（§2.3：
//     「放响应 _meta 作附加元数据」——SDK 内部构造结果无注入点，
//     只能在 HTTP 层补；解析失败原样透传，元数据是补充性的）。
//
// 缓冲是安全的：被包的只有「无会话 ID 的 POST」——长驻 SSE
// 流走带会话 ID 的 GET 分支，不经过这里。
type bufferWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (w *bufferWriter) WriteHeader(code int) { w.status = code }

func (w *bufferWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(b)
}

// Unwrap 透传底层 writer（ResponseController 查 Flush 等能力用）。
func (w *bufferWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// flush 把缓冲内容（可能注入了 _meta）写给真实 writer。
func (w *bufferWriter) flush(meta map[string]any) {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	body := w.buf.Bytes()
	if status < 400 && len(body) > 0 {
		if patched := injectMeta(body, meta); patched != nil {
			body = patched
		}
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(body)
}

// injectMeta 往 initialize 结果的 result 里塞 _meta。
// 覆盖两种响应形态：streamable SSE（`data: {...}` 行）与
// JSONResponse 的裸 JSON。解析不动返回 nil（原样透传）。
func injectMeta(body []byte, meta map[string]any) []byte {
	// SSE：逐 data: 行处理；本服务 initialize 只会有一条。
	if bytes.HasPrefix(body, []byte("event:")) {
		var out bytes.Buffer
		rest := body
		patched := false
		for len(rest) > 0 {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i+1], rest[i+1:]
			} else {
				rest = nil
			}
			if !patched && bytes.HasPrefix(line, []byte("data: ")) {
				end := bytes.IndexByte(line, '\n')
				if end < 0 {
					end = len(line)
				}
				if p := patchResultMeta(line[6:end], meta); p != nil {
					out.WriteString("data: ")
					out.Write(p)
					out.WriteByte('\n')
					patched = true
					continue
				}
			}
			out.Write(line)
		}
		if patched {
			return out.Bytes()
		}
		return nil
	}
	return patchResultMeta(body, meta)
}

// patchResultMeta 在 JSON-RPC 响应的 result 上写 _meta；
// 无 result 或解析失败返回 nil。
func patchResultMeta(data []byte, meta map[string]any) []byte {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil
	}
	res, ok := msg["result"].(map[string]any)
	if !ok {
		return nil
	}
	res["_meta"] = meta
	b, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// HTTP 服务装配
// ---------------------------------------------------------------------------

// HTTPOpts 是 NewHTTPHandler 的装配参数。
type HTTPOpts struct {
	Addr     string // 监听地址（如 127.0.0.1:8765），Host 白名单由此推导
	Token    string // Bearer 凭据；重新生成时调 SetToken 立即换值
	Provider BookProvider
	Version  string
	Operator string
	// OnChange 写工具成功后回调（desktop 用它发 mcp:changed）。
	OnChange func(entity string, id int64)
}

// HTTPService 是包好了鉴权与会代管理的 http.Handler 及其
// 凭据格子的组合；调用方持有它以便「重新生成凭据」。
type HTTPService struct {
	Handler http.Handler
	token   *tokenHolder
	table   *sessionTable
}

// SetToken 立刻更换凭据（旧值当即失效）。
func (h *HTTPService) SetToken(t string) { h.token.set(t) }

// NewHTTPService 按 §2.2/§2.3 装配四层拦截 + SDK streamable 处理链。
func NewHTTPService(opts HTTPOpts) (*HTTPService, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("HTTPOpts.Provider 不能为空")
	}
	if opts.Token == "" {
		return nil, fmt.Errorf("HTTPOpts.Token 不能为空")
	}
	hosts, err := allowedHosts(opts.Addr)
	if err != nil {
		return nil, err
	}

	srv := NewWithSource(providerSource{p: opts.Provider}, Options{
		Operator: opts.Operator,
		Version:  opts.Version,
	})
	srv.OnChange = opts.OnChange

	inner := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv.core },
		&mcp.StreamableHTTPOptions{
			// 世代层删掉的会话在 SDK 内部就成孤儿了，
			// 空闲超时负责把它们扫掉，免得越积越多。
			SessionTimeout: 30 * time.Minute,
		})

	h := &HTTPService{
		token: &tokenHolder{},
		table: newSessionTable(),
	}
	h.token.set(opts.Token)

	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Host —— 精确白名单，挡 DNS rebinding。
		if _, ok := hosts[r.Host]; !ok {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// 2. Origin —— 无头放行（非浏览器客户端）；
		//    有头必须解析得出 localhost/127.0.0.1 的主机名。
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		// 3. Token —— Bearer 精确比对（常数时间）。
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), []byte(h.token.get())) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="miniaccount-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// 4. 会话世代 —— 带会话 ID 的请求先过注册表。
		if sid := r.Header.Get(sessionHeader); sid != "" {
			if !h.table.check(sid, opts.Provider) {
				// 世代不符 → 按规范 404，客户端重新 initialize。
				http.Error(w, "session expired", http.StatusNotFound)
				return
			}
			inner.ServeHTTP(w, r)
			return
		}
		// 无会话 ID 的请求：initialize（或规范允许的会话建立
		// POST）会签出新会话 —— 缓冲响应，注入 _meta 元数据，
		// 同时把 (bookID, generation) 钉进注册表。
		bw := &bufferWriter{ResponseWriter: w}
		inner.ServeHTTP(bw, r)
		bookID, gen := opts.Provider.BookID(), opts.Provider.Generation()
		bw.flush(map[string]any{"bookId": bookID, "generation": gen})
		if sid := w.Header().Get(sessionHeader); sid != "" {
			h.table.put(sid, sessMeta{bookID: bookID, generation: gen})
		}
	})
	h.Handler = mux
	return h, nil
}

// allowedHosts 由监听地址推导 Host 白名单：
// 只精确放行 127.0.0.1:<port> 与 localhost:<port> 两个值。
func allowedHosts(addr string) (map[string]struct{}, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("监听地址 %q 不合法（需要 host:port）: %w", addr, err)
	}
	return map[string]struct{}{
		"127.0.0.1:" + port: {},
		"localhost:" + port: {},
	}, nil
}

// originAllowed 校验 Origin 头：解析出主机名后只放行
// localhost / 127.0.0.1；端口号不限（前端 dev server 端口会变）。
func originAllowed(o string) bool {
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1":
		return true
	}
	return false
}
