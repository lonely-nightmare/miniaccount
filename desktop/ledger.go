package main

import (
	"miniaccount/internal/service"
)

// ---------------------------------------------------------------------------
// 明细账
// ---------------------------------------------------------------------------

// LedgerRequest 是查明细账的参数。
type LedgerRequest = service.LedgerDetailQuery

// LedgerRow 是明细账的一行；定义在服务层，MCP 与 GUI 共用一份。
type LedgerRow = service.LedgerRow

// LedgerResult 是明细账的返回；定义在服务层。
type LedgerResult = service.LedgerResult

// LedgerDetail 返回某科目的明细账。
func (a *App) LedgerDetail(req LedgerRequest) (out *LedgerResult, err error) {
	defer recoverTo(&err, "LedgerDetail")()
	svc, f := a.book()
	if f != nil {
		return nil, f
	}
	res, err := svc.LedgerDetail(a.context(), req)
	if err != nil {
		return nil, classify(err)
	}
	return res, nil
}

// ★ 期初取数收进了 service.LedgerPrefixBalanceBefore ——
// 绑定层不该写 SQL：它不知道表结构，也不该知道。
