package service

import (
	"context"
	"fmt"
	"strings"

	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/domain/report"
	"miniaccount/internal/domain/voucher"
	"miniaccount/internal/store/sqlite"
)

// ---------------------------------------------------------------------------
// 凭证分页查询（MCP / 对外）
// ---------------------------------------------------------------------------

// VoucherPage 是一页凭证。
type VoucherPage struct {
	Items   []VoucherSummary `json:"items"`
	Total   int64            `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	HasMore bool             `json:"hasMore"`
}

// VouchersPage 分页查凭证；供 MCP query_vouchers 用。
// GUI 继续走 Vouchers（行为不变），这条是带 total 的对外契约。
func (s *Service) VouchersPage(ctx context.Context, q VoucherQuery) (*VoucherPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	f := sqlite.ListFilter{
		Year: q.Year, Month: q.Month, Status: q.Status,
		Keyword: q.Keyword, AccountPrefix: q.AccountPrefix,
		Limit: limit, Offset: offset,
	}
	total, err := s.db.Vouchers().ListCount(ctx, f)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Vouchers().List(ctx, f)
	if err != nil {
		return nil, err
	}
	stats, err := s.lineStats(ctx, rows)
	if err != nil {
		return nil, err
	}
	items := make([]VoucherSummary, 0, len(rows))
	for _, v := range rows {
		items = append(items, VoucherSummary{
			ID: v.ID, No: v.No, Word: string(v.Word), Date: v.BizDate.String(),
			Remark: v.Remark, Status: string(v.Status),
			StatusLabel: v.Status.Label(), AttachCount: v.AttachCount,
			// 列表只读凭证头，金额必须取分录合计——
			// 用 v.Amount() 永远是 0（分录没加载）。
			Amount: stats[v.ID].sum, Source: string(v.Source),
			SourceLabel: v.Source.Label(), CreatedByAI: v.CreatedByAI,
			CreatedBy: v.CreatedBy, PostedBy: v.PostedBy,
			Lines: stats[v.ID].count, UpdatedAt: v.UpdatedAtText,
		})
	}
	return &VoucherPage{
		Items: items, Total: total, Limit: limit, Offset: offset,
		HasMore: int64(offset)+int64(len(items)) < total,
	}, nil
}

// lineStat 是一张凭证的分录行数与借方合计（分）。
type lineStat struct {
	count int
	sum   money.Money
}

// lineStats 只统计返回页里这几张凭证的分录；
// 跨期间分页时按月统计会把整月几百张的行数全扫一遍，没必要。
func (s *Service) lineStats(ctx context.Context, vs []*voucher.Voucher) (map[int64]lineStat, error) {
	out := map[int64]lineStat{}
	if len(vs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(vs))
	ph = ph[:len(ph)-1]
	args := make([]any, len(vs))
	for i, v := range vs {
		args[i] = v.ID
	}
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT voucher_id, COUNT(*), COALESCE(SUM(debit),0) FROM voucher_entry WHERE voucher_id IN (`+ph+`) GROUP BY voucher_id`,
		args...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sum int64
		var n int
		if err := rows.Scan(&id, &n, &sum); err != nil {
			return nil, err
		}
		out[id] = lineStat{count: n, sum: money.Money(sum)}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 明细账
// ---------------------------------------------------------------------------

// LedgerDetailQuery 是查明细账的参数。
type LedgerDetailQuery struct {
	// AccountPrefix 是科目编码前缀，如 "1002"、"2241"。
	AccountPrefix string `json:"accountPrefix"`
	// From / To 是日期区间（YYYY-MM-DD），留空表示账套全期间。
	From string `json:"from"`
	To   string `json:"to"`
	// Limit 限制返回行数；0 表示默认上限。
	Limit int `json:"limit"`
}

// LedgerRow 是明细账的一行；Balance 是逐行累计余额（分，带符号，借正贷负）。
type LedgerRow struct {
	Date    string `json:"date"`
	Voucher string `json:"voucher"`
	Summary string `json:"summary"`
	Debit   int64  `json:"debit"`
	Credit  int64  `json:"credit"`
	Balance int64  `json:"balance"`
	// Dir 是该行余额方向：借 / 贷 / 平。
	Dir string `json:"dir"`
	// Contra 是对方科目名（「、」连接），日记账场景最要紧的一列。
	Contra string `json:"contra"`
}

// LedgerResult 是明细账的返回。
type LedgerResult struct {
	AccountPrefix  string      `json:"accountPrefix"`
	AccountName    string      `json:"accountName"`
	From           string      `json:"from"`
	To             string      `json:"to"`
	Rows           []LedgerRow `json:"rows"`
	OpeningBalance int64       `json:"openingBalance"`
	ClosingBalance int64       `json:"closingBalance"`
	Truncated      bool        `json:"truncated"`
}

// LedgerDetail 返回某科目的明细账（仅已过账分录，由 repo 保证）。
func (s *Service) LedgerDetail(ctx context.Context, q LedgerDetailQuery) (*LedgerResult, error) {
	prefix := strings.TrimSpace(q.AccountPrefix)
	if prefix == "" {
		return nil, &InvalidInput{Msg: "科目编码前缀不能为空，如 1002"}
	}
	from, to, err := s.resolveRange(ctx, q.From, q.To)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 2000
	}

	rows, err := s.db.Vouchers().Detail(ctx, prefix, from, to)
	if err != nil {
		return nil, err
	}
	result := &LedgerResult{
		AccountPrefix: prefix,
		From:          from.String(),
		To:            to.String(),
		Rows:          []LedgerRow{},
	}
	if acc, err := s.db.Accounts().GetByCode(ctx, prefix); err == nil {
		result.AccountName = acc.Name
	}
	opening, err := s.LedgerPrefixBalanceBefore(ctx, prefix, from)
	if err != nil {
		return nil, err
	}
	result.OpeningBalance = int64(opening)

	// 余额必须跑完整个区间 —— 在 limit 处停下会让
	// closingBalance 变成「截断位置的余额」，不是期末余额。
	balance := opening
	for _, r := range rows {
		balance = balance.Add(r.Debit).Sub(r.Credit)
		if len(result.Rows) >= limit {
			result.Truncated = true
			continue
		}
		result.Rows = append(result.Rows, LedgerRow{
			Date: r.BizDate.String(), Voucher: r.VoucherNo, Summary: r.Summary,
			Debit: int64(r.Debit), Credit: int64(r.Credit),
			Balance: int64(balance), Dir: balanceDirLabel(balance),
			Contra: strings.Join(r.ContraAccounts, "、"),
		})
	}
	result.ClosingBalance = int64(balance)
	return result, nil
}

// resolveRange 解析日期区间；留空时取账套的全部会计期间。
func (s *Service) resolveRange(ctx context.Context, from, to string) (calendar.Date, calendar.Date, error) {
	book, err := s.Book(ctx)
	if err != nil {
		return calendar.Date{}, calendar.Date{}, err
	}
	var dFrom, dTo calendar.Date
	if len(book.Periods) > 0 {
		if dFrom, err = calendar.Parse(book.Periods[0].From); err != nil {
			return calendar.Date{}, calendar.Date{}, err
		}
		if dTo, err = calendar.Parse(book.Periods[len(book.Periods)-1].To); err != nil {
			return calendar.Date{}, calendar.Date{}, err
		}
	} else {
		dFrom, dTo = calendar.Today(), calendar.Today()
	}
	if strings.TrimSpace(from) != "" {
		if dFrom, err = calendar.Parse(from); err != nil {
			return calendar.Date{}, calendar.Date{}, &InvalidInput{Msg: fmt.Sprintf("起始日期 %q 格式不对，应为 YYYY-MM-DD", from)}
		}
	}
	if strings.TrimSpace(to) != "" {
		if dTo, err = calendar.Parse(to); err != nil {
			return calendar.Date{}, calendar.Date{}, &InvalidInput{Msg: fmt.Sprintf("截止日期 %q 格式不对，应为 YYYY-MM-DD", to)}
		}
	}
	if dTo.Before(dFrom) {
		return calendar.Date{}, calendar.Date{}, &InvalidInput{Msg: "截止日期不能早于起始日期"}
	}
	return dFrom, dTo, nil
}

func balanceDirLabel(v money.Money) string {
	switch {
	case v.IsPositive():
		return "借"
	case v.IsNegative():
		return "贷"
	default:
		return "平"
	}
}

// ---------------------------------------------------------------------------
// 报表（试算平衡表 / 资产负债表 / 利润表）
// ---------------------------------------------------------------------------
//
// 只返回已过账分录 —— repo 层口径即如此，MCP 看到的就是账面上的数。

// TrialBalance 返回某期间的试算平衡表。
func (s *Service) TrialBalance(ctx context.Context, year, month int) (*report.BalanceReport, error) {
	return s.db.Reports().TrialBalanceReport(ctx, period.NewKey(year, month))
}

// BalanceSheet 返回截至某日的资产负债表（含勾稽检查问题清单）。
func (s *Service) BalanceSheet(ctx context.Context, asOf string) (
	closing, opening *report.Definition, issues []report.CheckIssue, err error) {
	d, err := calendar.Parse(asOf)
	if err != nil {
		return nil, nil, nil, &InvalidInput{Msg: fmt.Sprintf("报表日期 %q 格式不对，应为 YYYY-MM-DD", asOf)}
	}
	return s.db.Reports().BuildBalanceSheet(ctx, d)
}

// IncomeStatement 返回某期间的利润表（本期 + 本年累计，含勾稽问题清单）。
func (s *Service) IncomeStatement(ctx context.Context, year, month int) (
	current, ytd *report.Definition, issues []report.CheckIssue, err error) {
	return s.db.Reports().BuildIncomeStatement(ctx, period.NewKey(year, month))
}
