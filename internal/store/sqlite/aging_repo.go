package sqlite

import (
	"context"

	"miniaccount/internal/domain/account"
	"miniaccount/internal/domain/aging"
	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/money"
)

// AgingRepo 生成应收/应付账龄分析表。
type AgingRepo struct{ db *DB }

// Aging 返回账龄仓储。
func (db *DB) Aging() *AgingRepo { return &AgingRepo{db: db} }

// BuildAging 生成账龄分析表。
//
// accountPrefix 限定科目范围：传 "1122" 只看应收、"2202" 只看应付、
// 传空则看全部带往来辅助核算的科目。
//
// # 为什么只取带往来辅助核算的分录
//
// 账龄是按「某个客户 / 某个供应商欠了多久」算的。
// 一笔没有往来单位的分录（如费用科目的分摊）没有对象，
// 放进账龄表只会让合计对不上。
func (r *AgingRepo) BuildAging(ctx context.Context, asOf calendar.Date,
	accountPrefix string) (*aging.Report, error) {

	sqlText := `
		SELECT le.contact_id, a.code, a.name, le.biz_date,
		       le.debit, le.credit, le.summary, COALESCE(v.no, '')
		  FROM ledger_entry le
		  JOIN account a ON a.id = le.account_id
		  LEFT JOIN voucher v ON v.id = le.voucher_id
		 WHERE le.contact_id IS NOT NULL
		   AND le.biz_date <= ?`
	args := []any{asOf.String()}
	if accountPrefix != "" {
		sqlText += ` AND a.code LIKE ?`
		args = append(args, accountPrefix+"%")
	}
	sqlText += ` ORDER BY le.contact_id, a.code, le.biz_date, le.id`

	var (
		entries []aging.Entry
		names   = map[int64]string{}
		tree    *account.Tree
	)
	// ★ 一个只读事务包住分录、往来单位与科目树：
	// 账龄表发给客户/供应商对账，「分录归属」与「名称」分属两个瞬间
	// 时，同一笔款会对出两个名字或漏进刚建的新档案。
	err := r.db.Read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, sqlText, args...)
		if err != nil {
			return translateErr(err)
		}

		for rows.Next() {
			var (
				contactID     int64
				code, name    string
				bizDate       string
				debit, credit int64
				summary       string
				voucherNo     string
			)
			if err := rows.Scan(&contactID, &code, &name, &bizDate,
				&debit, &credit, &summary, &voucherNo); err != nil {
				rows.Close()
				return err
			}
			d, err := calendar.Parse(bizDate)
			if err != nil {
				rows.Close()
				return err
			}
			entries = append(entries, aging.Entry{
				ContactID: contactID, AccountCode: code, Date: d,
				Debit: money.Money(debit), Credit: money.Money(credit),
				Summary: summary, VoucherNo: voucherNo,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close() // ★ 单连接池：先关 rows 再发下一次查询

		// 往来单位名称
		nrows, err := q.Query(ctx, `SELECT id, name FROM contact`)
		if err != nil {
			return translateErr(err)
		}
		for nrows.Next() {
			var id int64
			var name string
			if err := nrows.Scan(&id, &name); err != nil {
				nrows.Close()
				return err
			}
			names[id] = name
		}
		if err := nrows.Err(); err != nil {
			nrows.Close()
			return err
		}
		nrows.Close()

		tree, err = r.db.Accounts().TreeQ(ctx, q)
		return err
	})
	if err != nil {
		return nil, err
	}
	return aging.Build(aging.Input{
		AsOf: asOf, Entries: entries, Accounts: tree, ContactNames: names,
	})
}
