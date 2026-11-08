package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"miniaccount/internal/mcpserver"
	"miniaccount/internal/service"
)

// cmdMCP 以 stdio 传输对外暴露账套的 MCP 能力。
//
// 协议消息走 stdout，所以本命令**禁止**向 stdout 打日志 ——
// 任何一行非 JSON-RPC 输出都会让客户端的协议解析废掉。
// 诊断信息只能去 stderr。
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	db := fs.String("db", "miniaccount.db", "账套文件路径（必须已初始化）")
	readonly := fs.Bool("readonly", false, "只读模式：以 mode=ro 打开账套，只注册读工具")
	operator := fs.String("operator", "mcp", "写操作在审计日志里的操作人署名")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// --db 必须指向已存在的账套文件：service.Open 对不存在的
	// 路径可能表现成「这是个待建账的新库」，静默造一个空账套
	// 再让 MCP 对着它读写，是最隐蔽的数据错误。
	st, err := os.Stat(*db)
	if err != nil || st.IsDir() {
		return fmt.Errorf("账套文件 %q 不存在（mcp 不会替你建账，请先用 init/界面建账）", *db)
	}

	// readonly 用只读连接打开：不写库、不跑迁移（设计文档 §2.12）。
	svc, err := service.Open(ctx, service.Options{Path: *db, ReadOnly: *readonly})
	if err != nil {
		return fmt.Errorf("打开账套失败: %w", err)
	}
	defer svc.Shutdown()

	srv := mcpserver.New(svc, mcpserver.Options{
		ReadOnly: *readonly,
		Operator: strings.TrimSpace(*operator),
		Version:  service.AppVersion,
	})
	fmt.Fprintf(os.Stderr, "miniaccount MCP（stdio）已就绪：%s，%s\n",
		*db, map[bool]string{true: "只读", false: "读写"}[*readonly])
	return srv.RunStdio(ctx)
}
