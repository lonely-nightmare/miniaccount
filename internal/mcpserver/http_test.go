package mcpserver_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"miniaccount/internal/domain/money"
	"miniaccount/internal/mcpserver"
	"miniaccount/internal/service"
)

func openTestBook(t *testing.T) *service.Service {
	t.Helper()
	svc, err := service.Open(context.Background(), service.Options{
		Path: filepath.Join(t.TempDir(), "t.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Shutdown() })
	if _, err := svc.CreateBook(context.Background(), service.CreateBookInput{
		CompanyName: "HTTP 测试公司", TaxType: "general",
		StartYear: 2025, StartMonth: 1, ThroughYear: 2025,
		CurrentYear: 2025, CurrentMonth: 3,
	}); err != nil {
		t.Fatal(err)
	}
	return svc
}

// fakeProvider 是最小的 BookProvider：账套固定，世代可调。
type fakeProvider struct {
	svc *service.Service
	gen atomic.Int64
}

func (p *fakeProvider) Call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	return fn(ctx, p.svc)
}
func (p *fakeProvider) BookSnapshot() (string, int64) {
	return "/tmp/book.db", p.gen.Load()
}
func (p *fakeProvider) bump() { p.gen.Add(1) }

func newHTTPTestService(t *testing.T, p *fakeProvider, token string) *mcpserver.HTTPService {
	t.Helper()
	h, err := mcpserver.NewHTTPService(mcpserver.HTTPOpts{
		Addr: "127.0.0.1:8765", Token: token, Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(t *testing.T, h http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Host 白名单：非 127.0.0.1/localhost:<port> 一律 403。
func TestHTTPHostRejected(t *testing.T) {
	p := &fakeProvider{}
	h := newHTTPTestService(t, p, "tok")

	rec := do(t, h.Handler, func(r *http.Request) { r.Host = "evil.example.com:8765" })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("恶意 Host 应 403，实际 %d", rec.Code)
	}

	for _, host := range []string{"127.0.0.1:8765", "localhost:8765"} {
		rec := do(t, h.Handler, func(r *http.Request) { r.Host = host })
		if rec.Code == http.StatusForbidden {
			t.Fatalf("合法 Host %s 不应 403", host)
		}
	}
	// 端口也必须精确：同主机不同端口不放行。
	rec = do(t, h.Handler, func(r *http.Request) { r.Host = "127.0.0.1:9999" })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("错误端口应 403，实际 %d", rec.Code)
	}
}

// Origin：无头放行；非 localhost 来源 403。
func TestHTTPOriginChecked(t *testing.T) {
	p := &fakeProvider{}
	h := newHTTPTestService(t, p, "tok")

	rec := do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example.com")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("外站 Origin 应 403，实际 %d", rec.Code)
	}
	for _, o := range []string{"http://localhost:5173", "http://127.0.0.1:8765"} {
		rec := do(t, h.Handler, func(r *http.Request) { r.Header.Set("Origin", o) })
		if rec.Code == http.StatusForbidden {
			t.Fatalf("本地 Origin %s 不应 403", o)
		}
	}
	rec = do(t, h.Handler, nil) // 无 Origin：curl/客户端库放行
	if rec.Code == http.StatusForbidden {
		t.Fatal("无 Origin 不应 403")
	}
}

// Token：缺失/错误 → 401；重新生成后旧值立刻失效。
func TestHTTPTokenChecked(t *testing.T) {
	p := &fakeProvider{}
	h := newHTTPTestService(t, p, "tok")

	rec := do(t, h.Handler, func(r *http.Request) { r.Header.Del("Authorization") })
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，实际 %d", rec.Code)
	}
	rec = do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错凭据应 401，实际 %d", rec.Code)
	}
	rec = do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Authorization", "bearer tok") // 大小写敏感
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Bearer 前缀应 401，实际 %d", rec.Code)
	}

	// 重新生成：旧值立即失效、新值立即可用。
	h.SetToken("tok2")
	rec = do(t, h.Handler, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧凭据应立即失效，实际 %d", rec.Code)
	}
	rec = do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tok2")
	})
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("新凭据应立即可用")
	}
}

// 会话世代：initialize 签发 session → 账套切换后同 ID 请求 404。
func TestHTTPSessionGeneration(t *testing.T) {
	svc := openTestBook(t)
	p := &fakeProvider{svc: svc}
	h := newHTTPTestService(t, p, "tok")

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", strings.NewReader(init))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, req)

	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize 应签发会话 ID（响应 %d: %s）", rec.Code, rec.Body.String())
	}

	// §2.3：initialize 响应 _meta 应带 {bookId, generation}。
	body := rec.Body.String()
	if !strings.Contains(body, `"_meta"`) {
		t.Fatalf("initialize 响应缺 _meta: %s", body)
	}
	if !strings.Contains(body, `"bookId":"/tmp/book.db"`) ||
		!strings.Contains(body, `"generation":0`) {
		t.Fatalf("_meta 应含 bookId+generation: %s", body)
	}

	// 世代未变：会话可用（ping 不应 404）。
	rec = do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Mcp-Session-Id", sid)
		r.Header.Set("Accept", "application/json, text/event-stream")
	})
	if rec.Code == http.StatusNotFound {
		t.Fatalf("有效会话不应 404: %s", rec.Body.String())
	}

	// 账套切换：同一会话 ID → 404。
	p.bump()
	rec = do(t, h.Handler, func(r *http.Request) {
		r.Header.Set("Mcp-Session-Id", sid)
		r.Header.Set("Accept", "application/json, text/event-stream")
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("账套切换后旧会话应 404，实际 %d", rec.Code)
	}
	// 再次确认是持久失效而非偶发。
	rec = do(t, h.Handler, func(r *http.Request) { r.Header.Set("Mcp-Session-Id", sid) })
	if rec.Code != http.StatusNotFound {
		t.Fatalf("过期会话应持续 404，实际 %d", rec.Code)
	}
}

// 请求体上限：工具声明单文件可传 20 MiB；SDK 默认 4 MiB
// 会把合法上传拦成 413（base64 膨胀后实际不到 3 MiB 文件）。
// 实测复现：4 MiB 文件走 HTTP 返回 413、走 stdio 成功。
func TestHTTPLargeUploadNotRejected(t *testing.T) {
	svc := openTestBook(t)
	p := &fakeProvider{svc: svc}
	h := newHTTPTestService(t, p, "tok")

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", strings.NewReader(init))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, req)
	sid := rec.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize 应签发会话: %s", rec.Body.String())
	}

	// 建一张凭证作归属。
	d, err := svc.SaveVoucher(context.Background(), service.VoucherInput{
		Word: "记", Date: "2025-01-10", Remark: "x", CreatedBy: "会计",
		Lines: []service.VoucherLineInput{
			{AccountCode: "1002", Summary: "借", Debit: money.Money(10000)},
			{AccountCode: "5001", Summary: "贷", Credit: money.Money(10000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4 MiB 文件 → base64 ~5.6 MiB —— 旧默认 4 MiB 必然 413。
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"upload_attachment","arguments":{"ownerType":"voucher","ownerId":` +
		strconv.FormatInt(d.ID, 10) +
		`,"fileName":"big.bin","dataBase64":"` + base64.StdEncoding.EncodeToString(payload) + `"}}}`
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/mcp", strings.NewReader(call))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	rec = httptest.NewRecorder()
	h.Handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("4 MiB 文件上传不应被 413 拒收 —— 请求体上限要与工具声明对齐")
	}
	if !strings.Contains(rec.Body.String(), `"hash"`) {
		t.Fatalf("上传应成功返回 hash，实际 %d: %.300s", rec.Code, rec.Body.String())
	}
}

// mcp.json：写入 0600 + 往返一致 + 坏文件报错。
func TestMCPConfigStore(t *testing.T) {
	store := &mcpserver.ConfigStore{Path: t.TempDir() + "/mcp.json"}
	tok, err := mcpserver.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("256-bit token 编码后应 ≥40 字符，实际 %d", len(tok))
	}
	if err := store.Save(&mcpserver.MCPConfig{Token: tok, Port: 8765, Operator: "出纳"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mcp.json 权限应为 0600，实际 %o", perm)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != tok || cfg.Port != 8765 || cfg.Operator != "出纳" {
		t.Fatalf("往返不一致: %+v", cfg)
	}
	// 吊销态（空 token）是合法配置：Load 不报错、原样返回。
	if err := store.Save(&mcpserver.MCPConfig{Port: 8765}); err != nil {
		t.Fatal(err)
	}
	cfg, err = store.Load()
	if err != nil || cfg.Token != "" {
		t.Fatalf("吊销态应能回读: cfg=%+v err=%v", cfg, err)
	}
	// 坏文件必须报错而非静默换凭据。
	if err := os.WriteFile(store.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("损坏的 mcp.json 应报错")
	}
}
