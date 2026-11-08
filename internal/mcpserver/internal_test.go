package mcpserver

// 世代复核：HTTP 中间件检查与真正取账套读锁之间存在
// 换账窗口 —— providerSource 必须在持锁临界区内按会话 ID
// 回查注册表再比一次，否则旧会话能把凭证写进新账套。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"miniaccount/internal/service"
)

type fakeBookProvider struct {
	svc    *service.Service
	gen    int64
	bookID string
}

func (f *fakeBookProvider) Call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	return fn(ctx, f.svc)
}
func (f *fakeBookProvider) BookID() string    { return f.bookID }
func (f *fakeBookProvider) Generation() int64 { return f.gen }

func TestProviderSourceRechecksGeneration(t *testing.T) {
	p := &fakeBookProvider{gen: 5, bookID: "/tmp/b.db"}
	table := newSessionTable()
	table.put("sid-a", sessMeta{bookID: "/tmp/b.db", generation: 5})
	src := providerSource{p: p, table: table}
	fn := func(context.Context, *service.Service) (any, error) { return "ok", nil }

	withSid := func(sid string) context.Context {
		return context.WithValue(context.Background(), sessionIDKey{}, sid)
	}

	// 钉住世代与当前一致：放行。
	if v, err := src.call(withSid("sid-a"), fn); err != nil || v != "ok" {
		t.Fatalf("匹配世代应放行: v=%v err=%v", v, err)
	}
	// 账套切换到 gen 6，表里的钉住还是 5：锁内复核必须拒绝。
	p.gen = 6
	if _, err := src.call(withSid("sid-a"), fn); !errors.Is(err, errStaleSession) {
		t.Fatalf("旧世代应被拒: %v", err)
	}
	// 换账路径不同（Restore 换文件）：bookID 不符同样拒绝。
	p.gen = 5
	p.bookID = "/tmp/c.db"
	if _, err := src.call(withSid("sid-a"), fn); !errors.Is(err, errStaleSession) {
		t.Fatalf("异账套应被拒: %v", err)
	}
	// 表里没有该会话（被 check 清掉/从未注册）：拒绝。
	p.bookID = "/tmp/b.db"
	if _, err := src.call(withSid("sid-gone"), fn); !errors.Is(err, errStaleSession) {
		t.Fatalf("未注册会话应被拒: %v", err)
	}
	// 无会话 ID（stdio/非会话路径）：放行。
	if v, err := src.call(context.Background(), fn); err != nil || v != "ok" {
		t.Fatalf("无会话应放行: v=%v err=%v", v, err)
	}
}

// ---------------------------------------------------------------------------
// 端到端：会话 ID 经 SDK 会话派发真的到达 Call 的 ctx。
//
// 病灶回顾：SDK 把 POST 推进会话 chan、在「initialize 请求
// 的 ctx（detached）」里派发 handler —— 往 POST 请求 ctx
// 塞值根本到不了工具调用。唯一通路是 handler 里
// req.Session.ID()（streamable conn 的 SessionID 就是
// Mcp-Session-Id 头值），run 把它放进调用 ctx。
// ---------------------------------------------------------------------------

// ctxProbe 捕获 Call 上下文里的会话 ID；
// bumpInCall 模拟「中间件检查之后、取锁之前」的换账窗口。
type ctxProbe struct {
	svc        *service.Service
	bookID     string
	gen        atomic.Int64
	bumpInCall atomic.Bool

	mu  sync.Mutex
	sid string
	got bool
}

func (p *ctxProbe) Call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	if p.bumpInCall.CompareAndSwap(true, false) {
		p.gen.Add(1)
	}
	p.mu.Lock()
	p.sid, p.got = ctx.Value(sessionIDKey{}).(string)
	p.mu.Unlock()
	return fn(ctx, p.svc)
}
func (p *ctxProbe) BookID() string    { return p.bookID }
func (p *ctxProbe) Generation() int64 { return p.gen.Load() }

func openInternalBook(t *testing.T) *service.Service {
	t.Helper()
	svc, err := service.Open(context.Background(), service.Options{
		Path: filepath.Join(t.TempDir(), "t.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Shutdown() })
	return svc
}

func TestHTTPSessionIDReachesCall(t *testing.T) {
	svc := openInternalBook(t)
	p := &ctxProbe{svc: svc, bookID: "/tmp/book.db"}
	h, err := NewHTTPService(HTTPOpts{
		Addr: "127.0.0.1:8765", Token: "tok", Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}

	post := func(body, sid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp",
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		rec := httptest.NewRecorder()
		h.Handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, "")
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize 应签发会话 ID: %d %s", rec.Code, rec.Body.String())
	}

	// 正常调用：会话 ID 必须顺着 req.Session 到达 Call 的 ctx。
	rec = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"book_info","arguments":{}}}`, sid)
	if rec.Code != http.StatusOK {
		t.Fatalf("book_info 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	p.mu.Lock()
	gotSid, gotFlag := p.sid, p.got
	p.mu.Unlock()
	if !gotFlag || gotSid != sid {
		t.Fatalf("会话 ID 应经 Session.ID() 到达 Call ctx: got=%q,%v 期望 %q",
			gotSid, gotFlag, sid)
	}

	// 换账窗口：Call 内部世代 +1（等价于检查→取锁之间切账）。
	p.bumpInCall.Store(true)
	rec = post(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"book_info","arguments":{}}}`, sid)
	if !strings.Contains(rec.Body.String(), "session_stale") &&
		!strings.Contains(rec.Body.String(), "账套已切换") {
		t.Fatalf("窗口内换账应被锁内复核拒绝: %d %s", rec.Code, rec.Body.String())
	}
}
