package sqlite

import (
	"context"

	"miniaccount/internal/domain/account"
	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/statement"
)

// StatementRepo 生成往来对账单。
type StatementRepo struct{ db *DB }

// Statements 返回对账单仓储。
func (db *DB) Statements() *StatementRepo { return &StatementRepo{db: db} }

// ContactInfo 是开对账单需要的对方信息。
type ContactInfo struct {
	ID          int64
	Name        string
	Kind        string
	TaxNo       string
	Address     string
	BankName    string
	BankAccount string
}

// ContactInfoOf 按 id 取往来单位信息。
func (r *StatementRepo) ContactInfoOf(ctx context.Context, id int64) (*ContactInfo, error) {
	var c ContactInfo
	err := r.db.sql.QueryRowContext(ctx, `
		SELECT id, name, kind, tax_no, address, bank_name, bank_account
		  FROM contact WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Kind, &c.TaxNo, &c.Address, &c.BankName, &c.BankAccount)
	if err != nil {
		return nil, translateErr(err)
	}
	return &c, nil
}

// BuildStatement 生成一张对账单。
//
// accountPrefix 限定科目范围（空表示该往来单位名下的全部科目）。
func (r *StatementRepo) BuildStatement(ctx context.Context, contactID int64,
	from, to calendar.Date, accountPrefix string) (*statement.Statement, []statement.Entry, error) {

	// 期间内的发生额
	sqlText := `
		SELECT a.code, le.biz_date, le.debit, le.credit, le.summary,
		       COALESCE(v.no, '')
		  FROM ledger_entry le
		  JOIN account a ON a.id = le.account_id
		  LEFT JOIN voucher v ON v.id = le.voucher_id
		 WHERE le.contact_id = ?
		   AND le.biz_date BETWEEN ? AND ?`
	args := []any{contactID, from.String(), to.String()}
	if accountPrefix != "" {
		sqlText += ` AND a.code LIKE ?`
		args = append(args, accountPrefix+"%")
	}
	sqlText += ` ORDER BY le.biz_date, v.no, le.id`

	var (
		opening money.Money
		entries []statement.Entry
		tree    *account.Tree
	)
	// ★ 期初余额与期间分录共用一份快照：
	// 两笔查询之间过账一张期初日之前的凭证，会出现
	// 「期初不含它、明细里也没有它」的丢钱对账单 ——
	// 而这张单子是要发给客户盖章确认的。
	err := r.db.Read(ctx, func(q Querier) error {
		// 期初余额 = 期间之前的全部发生额
		var e error
		if opening, e = r.contactBalanceBefore(ctx, q, contactID, from, accountPrefix); e != nil {
			return e
		}
		rows, err := q.Query(ctx, sqlText, args...)
		if err != nil {
			return translateErr(err)
		}
		for rows.Next() {
			var (
				code, bizDate, summary, voucherNo string
				debit, credit                     int64
			)
			if err := rows.Scan(&code, &bizDate, &debit, &credit, &summary, &voucherNo); err != nil {
				rows.Close()
				return err
			}
			d, err := calendar.Parse(bizDate)
			if err != nil {
				rows.Close()
				return err
			}
			entries = append(entries, statement.Entry{
				AccountCode: code, Date: d,
				Debit: money.Money(debit), Credit: money.Money(credit),
				Summary: summary, VoucherNo: voucherNo,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close() // ★ 单连接池：先关 rows 再继续查询
		tree, e = r.db.Accounts().TreeQ(ctx, q)
		return e
	})
	if err != nil {
		return nil, nil, err
	}

	names := map[string]string{}
	for _, e := range entries {
		if _, ok := names[e.AccountCode]; ok {
			continue
		}
		if a, ok := tree.Get(e.AccountCode); ok {
			names[e.AccountCode] = a.Name
		}
	}

	info, err := r.ContactInfoOf(ctx, contactID)
	if err != nil {
		return nil, nil, err
	}
	book, err := r.db.Books().Get(ctx)
	if err != nil {
		return nil, nil, err
	}

	st, err := statement.Build(statement.Input{
		CompanyName: book.CompanyName,
		ContactID:   contactID, ContactName: info.Name, ContactKind: info.Kind,
		ContactTaxNo: info.TaxNo, ContactAddress: info.Address,
		From: from, To: to, Entries: entries, Opening: opening,
		AccountNames: names, Accounts: tree,
	})
	if err != nil {
		return nil, nil, err
	}
	return st, entries, nil
}

// contactBalanceBefore 返回某往来单位在给定日期之前的净额（借−贷）。
func (r *StatementRepo) contactBalanceBefore(ctx context.Context, q Querier, contactID int64,
	before calendar.Date, accountPrefix string) (money.Money, error) {

	sqlText := `
		SELECT COALESCE(SUM(le.debit), 0) - COALESCE(SUM(le.credit), 0)
		  FROM ledger_entry le
		  JOIN account a ON a.id = le.account_id
		 WHERE le.contact_id = ? AND le.biz_date < ?`
	args := []any{contactID, before.String()}
	if accountPrefix != "" {
		sqlText += ` AND a.code LIKE ?`
		args = append(args, accountPrefix+"%")
	}

	var net int64
	if err := q.QueryRow(ctx, sqlText, args...).Scan(&net); err != nil {
		return 0, translateErr(err)
	}
	return money.Money(net), nil
}

// ContactsWithActivity 返回在给定期间内有往来发生的单位。
//
// 用于「批量开对账单」：会计到月底不需要一个个去挑，
// 系统直接告诉他这个月哪些客户有往来。
func (r *StatementRepo) ContactsWithActivity(ctx context.Context,
	from, to calendar.Date) ([]ContactInfo, error) {

	rows, err := r.db.sql.QueryContext(ctx, `
		SELECT DISTINCT c.id, c.name, c.kind, c.tax_no, c.address,
		       c.bank_name, c.bank_account
		  FROM ledger_entry le
		  JOIN contact c ON c.id = le.contact_id
		 WHERE le.biz_date BETWEEN ? AND ?
		 ORDER BY c.name`, from.String(), to.String())
	if err != nil {
		return nil, translateErr(err)
	}
	defer rows.Close()

	var out []ContactInfo
	for rows.Next() {
		var c ContactInfo
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.TaxNo, &c.Address,
			&c.BankName, &c.BankAccount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
