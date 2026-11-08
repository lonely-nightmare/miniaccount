package service

import (
	"context"
	"strings"

	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/money"
)

// LedgerPrefixBalanceBefore 返回某科目前缀在给定日期之前的累计净额（借−贷）。
//
// 明细账的「期初余额」用这个取：区间开始前的全部历史发生额轧成一个数。
// 原来这条 SQL 写在桌面绑定层 —— 绑定层不该知道表结构，
// 收进 service 后存储层仍是唯一碰 SQL 的地方。
func (s *Service) LedgerPrefixBalanceBefore(ctx context.Context,
	prefix string, before calendar.Date) (money.Money, error) {

	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return 0, nil
	}
	var net int64
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(le.debit), 0) - COALESCE(SUM(le.credit), 0)
		  FROM ledger_entry le
		  JOIN account a ON a.id = le.account_id
		 WHERE a.code LIKE ? AND le.biz_date < ?`,
		prefix+"%", before.String()).Scan(&net)
	if err != nil {
		return 0, translate(err)
	}
	return money.Money(net), nil
}
