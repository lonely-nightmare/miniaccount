package mcpserver

// 世代复核：HTTP 中间件检查与真正取账套读锁之间存在
// 换账窗口 —— providerSource 必须在持锁临界区内再比一次，
// 否则旧会话能把凭证写进新账套。

import (
	"context"
	"errors"
	"testing"

	"miniaccount/internal/service"
)

type fakeBookProvider struct {
	svc *service.Service
	gen int64
}

func (f *fakeBookProvider) Call(ctx context.Context,
	fn func(ctx context.Context, svc *service.Service) (any, error)) (any, error) {
	return fn(ctx, f.svc)
}
func (f *fakeBookProvider) BookID() string    { return "/tmp/b.db" }
func (f *fakeBookProvider) Generation() int64 { return f.gen }

func TestProviderSourceRechecksGeneration(t *testing.T) {
	p := &fakeBookProvider{gen: 5}
	src := providerSource{p: p}
	fn := func(context.Context, *service.Service) (any, error) { return "ok", nil }

	// 钉住旧世代：锁内复核必须拒绝。
	ctx := context.WithValue(context.Background(), pinnedGenKey{}, int64(4))
	if _, err := src.call(ctx, fn); !errors.Is(err, errStaleSession) {
		t.Fatalf("旧世代应被拒: %v", err)
	}
	// 匹配世代：放行。
	ctx = context.WithValue(context.Background(), pinnedGenKey{}, int64(5))
	if v, err := src.call(ctx, fn); err != nil || v != "ok" {
		t.Fatalf("匹配世代应放行: v=%v err=%v", v, err)
	}
	// 无钉住（stdio/非会话路径）：放行。
	if v, err := src.call(context.Background(), fn); err != nil || v != "ok" {
		t.Fatalf("无钉住应放行: v=%v err=%v", v, err)
	}
}
