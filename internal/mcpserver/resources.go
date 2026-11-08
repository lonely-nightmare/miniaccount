package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/service"
)

// registerResources 注册三个只读资源；内容是工具返回的 JSON 快照，
// 让模型在决定调用哪个工具前就能先看懂账套形状。
func (s *Server) registerResources() {
	read := func(uri, name, desc string,
		f func(ctx context.Context) (any, error)) {
		s.core.AddResource(&mcp.Resource{
			URI: uri, Name: name, Description: desc,
			MIMEType: "application/json",
		}, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			v, err := f(ctx)
			if err != nil {
				return nil, err
			}
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return nil, err
			}
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI: uri, MIMEType: "application/json", Text: string(b),
				}},
			}, nil
		})
	}

	read("book://info", "账套信息",
		"账套名称、公司、纳税人身份与会计期间一览",
		func(ctx context.Context) (any, error) {
			return s.src.call(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
				return svc.Book(ctx)
			})
		})

	read("book://accounts", "科目表",
		"全部会计科目：编码、名称、类别与辅助核算要求",
		func(ctx context.Context) (any, error) {
			return s.src.call(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
				return svc.Accounts(ctx)
			})
		})

	read("book://periods", "会计期间",
		"各会计期间的起止与状态（未启用/启用/已结账）",
		func(ctx context.Context) (any, error) {
			return s.src.call(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
				b, err := svc.Book(ctx)
				if err != nil {
					return nil, err
				}
				return b.Periods, nil
			})
		})
}
