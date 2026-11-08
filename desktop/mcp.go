package main

// ---------------------------------------------------------------------------
// MCP HTTP 服务（设计文档 §2.2/§2.3/§2.11）
//
// 账套世代：a.mu 保护 a.svc/a.path，setBookLocked 每次换账套把
// bookGen 加一 —— HTTP 层会话表钉住 initialize 时的世代，
// 换账后旧 MCP-Session-Id 一律 404，客户端按规范重新 initialize。
//
// 读锁范围：provider.Call 把 App.mu.RLock 覆盖到**一次业务调用**
// 上，绝不覆盖整个 HTTP 连接；Open/Close/Restore 拿 a.mu.Lock
// 时自然排在在途调用之后（写锁排队即排空）。
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"miniaccount/internal/mcpserver"
	"miniaccount/internal/service"
)

// ---------------------------------------------------------------- 账套提供方

// bookProvider 把 App 适配成 mcpserver.BookProvider。
// 方法必须叫 Call/BookID/Generation —— 但它们**不能**直接挂在
// App 上：Wails 会把 App 的全部导出方法绑成前端 API，
// 这三个名字会平白多出三个无意义绑定。包一层非导出类型即可。
type bookProvider struct{ a *App }

// Call 实现 mcpserver.BookProvider：读锁覆盖一次业务调用。
func (p bookProvider) Call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	p.a.mu.RLock()
	defer p.a.mu.RUnlock()
	if p.a.svc == nil {
		return nil, service.ErrNoBook
	}
	return fn(ctx, p.a.svc)
}

// BookID 返回当前账套路径（空串 = 未打开账套）。
func (p bookProvider) BookID() string {
	p.a.mu.RLock()
	defer p.a.mu.RUnlock()
	return p.a.path
}

// Generation 返回账套世代；Open/Close/Restore 换账套时递增。
func (p bookProvider) Generation() int64 { return p.a.bookGen.Load() }

// setBookLocked 在 a.mu.Lock 内换账套并推进世代。
// 调用方必须已持有写锁。
func (a *App) setBookLocked(svc *service.Service, path string) {
	a.svc, a.path = svc, path
	a.bookGen.Add(1)
}

// ---------------------------------------------------------------- 服务状态

// mcpRuntime 是运行中的 HTTP 服务句柄。
type mcpRuntime struct {
	srv *http.Server
	svc *mcpserver.HTTPService
	ln  net.Listener
}

// MCPServiceStatus 是设置页「MCP 服务」卡的数据。
type MCPServiceStatus struct {
	Running  bool   `json:"running"`
	Port     int    `json:"port"`
	Operator string `json:"operator"`
	Token    string `json:"token"`
	URL      string `json:"url"`
}

// MCPStartRequest 是开启服务的请求。
type MCPStartRequest struct {
	Port     int    `json:"port"`
	Operator string `json:"operator"`
}

// configStore 返回 mcp.json 存储；错误只在调用点处理。
func (a *App) configStore() (*mcpserver.ConfigStore, error) {
	return mcpserver.DefaultConfigStore()
}

// MCPStatus 返回当前 MCP 服务状态（含凭据，供复制按钮用）。
func (a *App) MCPStatus() (out MCPServiceStatus, err error) {
	defer recoverTo(&err, "MCPStatus")()
	a.mu.RLock()
	rt := a.mcp
	a.mu.RUnlock()

	st, serr := a.configStore()
	if serr != nil {
		return out, classify(serr)
	}
	cfg, cerr := st.Load()
	if cerr != nil {
		return out, classify(cerr)
	}
	out.Running = rt != nil
	if cfg != nil {
		out.Port, out.Operator, out.Token = cfg.Port, cfg.Operator, cfg.Token
	}
	if out.Running {
		out.URL = "http://127.0.0.1:" + fmt.Sprint(out.Port) + "/mcp"
	}
	return out, nil
}

// MCPStart 开启 HTTP MCP 服务：加载或生成凭据 → 起 127.0.0.1 监听。
// 已运行时先停再开（端口/操作人可能变了）。
func (a *App) MCPStart(req MCPStartRequest) (out MCPServiceStatus, err error) {
	defer recoverTo(&err, "MCPStart")()
	if req.Port <= 0 || req.Port > 65535 {
		return out, &Fault{Kind: FaultInvalid, Message: "端口必须是 1-65535"}
	}

	st, serr := a.configStore()
	if serr != nil {
		return out, classify(serr)
	}
	cfg, cerr := st.Load()
	if cerr != nil {
		return out, classify(cerr)
	}
	if cfg == nil {
		cfg = &mcpserver.MCPConfig{}
	}
	// 没有凭据（首次开启或上次关闭时吊销）就签发新的；
	// 已有凭据沿用 —— 重开服务不换凭据，「重新生成」按钮才换。
	if cfg.Token == "" {
		tok, terr := mcpserver.GenerateToken()
		if terr != nil {
			return out, classify(terr)
		}
		cfg.Token = tok
	}
	cfg.Port = req.Port
	if req.Operator != "" {
		cfg.Operator = req.Operator
	}
	if cfg.Operator == "" {
		cfg.Operator = "mcp"
	}
	if err := st.Save(cfg); err != nil {
		return out, classify(err)
	}

	_ = a.stopMCP() // 幂等：先收掉旧的（如有）

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	hsvc, herr := mcpserver.NewHTTPService(mcpserver.HTTPOpts{
		Addr: addr, Token: cfg.Token, Provider: bookProvider{a},
		Version: service.AppVersion, Operator: cfg.Operator,
		OnChange: func(entity string, id int64) {
			if a.ctx != nil {
				runtime.EventsEmit(a.ctx, "mcp:changed",
					map[string]any{"entity": entity, "id": id})
			}
		},
	})
	if herr != nil {
		return out, classify(herr)
	}
	ln, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		return out, classify(lerr)
	}
	httpSrv := &http.Server{Handler: hsvc.Handler}
	go func() { _ = httpSrv.Serve(ln) }()

	a.mu.Lock()
	a.mcp = &mcpRuntime{srv: httpSrv, svc: hsvc, ln: ln}
	a.mu.Unlock()
	return a.MCPStatus()
}

// MCPStop 关闭服务并按文档吊销凭据（关闭即失效）。
//
// 顺序是「先吊销、后关停」：Shutdown 要等 SSE 长连接排空，
// 可能超时返回——吊销若放在后面，服务已标记停止但旧凭据
// 还留在 mcp.json 里，下次开启会继续用。
func (a *App) MCPStop() (err error) {
	defer recoverTo(&err, "MCPStop")()
	st, serr := a.configStore()
	if serr != nil {
		return classify(serr)
	}
	cfg, _ := st.Load()
	if cfg == nil {
		cfg = &mcpserver.MCPConfig{}
	}
	// 1. 持久化吊销：mcp.json 的 token 清空，下次开启重签。
	//    这步失败就停下来——继续关停会留下「文件里还是旧
	//    凭据」的状态，重启后旧凭据复活。
	cfg.Token = ""
	if err := st.Save(cfg); err != nil {
		return classify(err)
	}
	// 2. 运行中的实例立刻换值（旧凭据当即不可用）；
	//    随机值失败时退空串——空 token 比对任何 Bearer 都不等。
	if rt := a.currentMCP(); rt != nil {
		if tok, terr := mcpserver.GenerateToken(); terr == nil {
			rt.svc.SetToken(tok)
		} else {
			rt.svc.SetToken("")
		}
	}
	// 3. 关停排空；超时被拒的连接已在第 2 步死了。
	if err := a.stopMCP(); err != nil {
		return classify(err)
	}
	return nil
}

// currentMCP 读当前运行句柄（不摘指针）。
func (a *App) currentMCP() *mcpRuntime {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mcp
}

// stopMCP 停掉运行中的服务；未运行是空操作。
// 「摘指针」与「关停」分离：http.Server.Shutdown 要等连接排空，
// 持账套锁做会自锁（shutdown 已在 a.mu 内时尤其明显）。
func (a *App) stopMCP() error {
	a.mu.Lock()
	rt := a.mcp
	a.mcp = nil
	a.mu.Unlock()
	return shutdownMCP(rt)
}

// takeMCPLocked 在已持 a.mu 的临界区内摘掉服务句柄；
// 调用方在锁外调 shutdownMCP 完成关停。
func (a *App) takeMCPLocked() *mcpRuntime {
	rt := a.mcp
	a.mcp = nil
	return rt
}

// shutdownMCP 等 HTTP 服务排空退出；不持任何锁。
// SSE 长连接可能拖过 3s 排空窗口 —— 超时后强制 Close，
// 不让残留连接把「已停止」拖成半开状态。
func shutdownMCP(rt *mcpRuntime) error {
	if rt == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rt.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_ = rt.srv.Close()
		return err
	}
	return nil
}

// MCPRegenerateToken 立即签发新凭据并吊销旧值；返回新凭据供一次性展示。
func (a *App) MCPRegenerateToken() (out MCPServiceStatus, err error) {
	defer recoverTo(&err, "MCPRegenerateToken")()
	st, serr := a.configStore()
	if serr != nil {
		return out, classify(serr)
	}
	tok, terr := mcpserver.GenerateToken()
	if terr != nil {
		return out, classify(terr)
	}
	cfg, _ := st.Load()
	if cfg == nil {
		cfg = &mcpserver.MCPConfig{}
	}
	cfg.Token = tok
	if err := st.Save(cfg); err != nil {
		return out, classify(err)
	}
	a.mu.RLock()
	rt := a.mcp
	a.mu.RUnlock()
	if rt != nil {
		rt.svc.SetToken(tok) // 运行中即刻生效
	}
	return a.MCPStatus()
}

// 保证包装类型实现 mcpserver.BookProvider。
var _ mcpserver.BookProvider = bookProvider{}
