package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 只看一眼：这个文件是不是账套、是哪家公司的
// ---------------------------------------------------------------------------
//
// ★ 用**只读**连接，而且**绝不跑迁移**。
//
// 启动时要列出账套目录里的文件，逐个判断「是不是账套」。
// 如果这里复用正常的 Open，就会对那些根本不是账套的文件
// （用户误放的备份、别的软件的库、空文件）执行建表 ——
// 等于我们主动往用户的文件里写东西。列个目录而已，不该有副作用。
//
// 只读打开还有一个好处：账套正在被另一个进程使用时（比如用户开了两个窗口），
// 这里读得到、不会抢锁、也不会把 WAL 搅乱。

// BookPeek 是一个账套文件的外观信息。
type BookPeek struct {
	// Path 是文件的绝对路径。
	Path string `json:"path"`
	// FileName 是文件名，界面直接显示。
	FileName string `json:"fileName"`
	// IsBook 为真表示这个文件确实是一个账套（有 book 表且有记录）。
	IsBook bool `json:"isBook"`
	// CompanyName 等单位信息仅在 IsBook 为真时有意义。
	CompanyName string `json:"companyName"`
	CreditCode  string `json:"creditCode"`
	VATStatus   string `json:"vatStatus"`
	// StartPeriod 是启用期间，如 2025-01。
	StartPeriod string `json:"startPeriod"`
	// VoucherCount 是凭证张数 —— 用户靠它分辨「哪本是正式账」。
	VoucherCount int `json:"voucherCount"`
	// SizeBytes / ModTime 用于排序与显示。
	SizeBytes int64  `json:"sizeBytes"`
	ModTime   string `json:"modTime"`
	// Err 说明为什么这个文件不能当账套用（不是账套/读不了/版本太新）。
	// 有值时 IsBook 必为假。
	Err string `json:"err,omitempty"`
}

// PeekBook 只读地看一眼某个文件是不是账套。
//
// 任何失败都写进返回值的 Err 字段而不是返回 error：
// 列目录时一个坏文件不该让整个列表失败 —— 用户还要打开别的好账套。
func PeekBook(ctx context.Context, path string) BookPeek {
	p := BookPeek{Path: path, FileName: filepath.Base(path)}

	fi, err := os.Stat(path)
	if err != nil {
		p.Err = "读不到这个文件：" + err.Error()
		return p
	}
	if fi.IsDir() {
		p.Err = "这是一个目录，不是账套文件"
		return p
	}
	p.SizeBytes = fi.Size()
	p.ModTime = fi.ModTime().Format(time.RFC3339)

	if fi.Size() == 0 {
		p.Err = "空文件（还没建过账）"
		return p
	}

	db, err := Open(ctx, Options{Path: path, ReadOnly: true})
	if err != nil {
		p.Err = "打不开：" + err.Error()
		// 只读类错误再给半句提示：文件在只读介质/目录上、或正被
		// 另一个进程以独占方式占用时，用户看到的是一句干巴巴的
		// 「打不开」，完全不知道该查什么。
		if strings.Contains(err.Error(), "readonly") ||
			strings.Contains(err.Error(), "read-only") {
			p.Err += "（文件或所在目录可能是只读的）"
		}
		return p
	}
	defer func() { _ = db.Close() }()

	if !db.hasTable(ctx, "book") {
		p.Err = "不是账套文件（没有 book 表）"
		return p
	}

	// ★ 只选「这库实际有的列」。
	//
	// vat_status 等列是后加的（迁移 0010），0010 之前建的老账套
	// 没有它们 —— 直接 SELECT 会报「no such column」，
	// 老账套在选择器里就全被当成「不是账套」，
	// 尽管正常打开路径能把它们迁移升级。
	cols, err := db.columnNames(ctx, "book")
	if err != nil {
		p.Err = "读不出表结构：" + err.Error()
		return p
	}
	pick := func(name string) string {
		if cols[name] {
			return name
		}
		return "NULL AS " + name
	}

	var (
		name, credit, vat     sql.NullString
		startYear, startMonth sql.NullInt64
		setup                 sql.NullInt64
		vouchers              int
	)
	err = db.sql.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT %s, %s, %s, %s, %s, %s
		  FROM book WHERE id = 1`,
		pick("company_name"), pick("credit_code"), pick("vat_status"),
		pick("start_year"), pick("start_month"),
		"COALESCE(setup_complete, 0)")).
		Scan(&name, &credit, &vat, &startYear, &startMonth, &setup)
	if errors.Is(err, sql.ErrNoRows) {
		p.Err = "还是空库（建账没做完）"
		return p
	}
	if err != nil {
		p.Err = "读不出账套信息：" + err.Error()
		return p
	}

	p.IsBook = true
	p.CompanyName = name.String
	p.CreditCode = credit.String
	p.VATStatus = vat.String
	if startYear.Valid && startMonth.Valid {
		p.StartPeriod = fmt.Sprintf("%04d-%02d", startYear.Int64, startMonth.Int64)
	}
	// 凭证数读不出来不算「不是账套」：老版本可能没有这张表
	_ = db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM voucher`).Scan(&vouchers)
	p.VoucherCount = vouchers
	return p
}

// hasTable 判断库里有没有某张表。
func (db *DB) hasTable(ctx context.Context, name string) bool {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name).Scan(&n)
	return err == nil && n > 0
}

// columnNames 返回某张表的全部列名（供兼容性探测用）。
func (db *DB) columnNames(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := db.sql.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, translateErr(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid         int
			name, typ   string
			notNull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ScanBookDir 列出目录下所有 .db 文件的外观信息，按修改时间倒序。
//
// 目录不存在时返回空列表而不是错误：第一次使用软件时那个目录还没建，
// 这不是异常。
func ScanBookDir(ctx context.Context, dir string) ([]BookPeek, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, translateErr(err)
	}
	var out []BookPeek
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// 只看 .db；WAL/SHM 边车文件不能当账套列出来
		if !strings.EqualFold(filepath.Ext(e.Name()), ".db") {
			continue
		}
		out = append(out, PeekBook(ctx, filepath.Join(dir, e.Name())))
	}
	sort.SliceStable(out, func(i, j int) bool {
		// 账套在前；同类按修改时间倒序（最近在用的更可能是要打开的那个）
		if out[i].IsBook != out[j].IsBook {
			return out[i].IsBook
		}
		return out[i].ModTime > out[j].ModTime
	})
	return out, nil
}

// VerifyOpenable 校验一个账套文件能被本程序打开。
//
// # 为什么是「拷一份临时副本、用正常 Open 全真开一遍」
//
// 只读打开只能回答「文件有没有坏、版本是不是太新」，
// 回答不了「这套库能不能被迁移成当前程序要的形状」——
// 真实案例：备份的 schema_migration 缺一条记录，
// 只读检查通过、真打开时迁移在重复列上崩溃，
// 而原账套已经被挪进暂存目录。
//
// 在副本上跑**和真实打开完全相同**的 Open（含全部迁移），
// 副本开得起，真文件就开得起；副本开不起，
// 恢复流程回滚、原账套原样奉还。迁移写进的是副本，
// 真实文件保持备份原样，由之后正常打开再迁移一遍。
func VerifyOpenable(ctx context.Context, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取待验证账套: %w", err)
	}
	tmp, err := os.CreateTemp("", "miniaccount-verify-*.db")
	if err != nil {
		return fmt.Errorf("建验证副本: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		// WAL/SHM 是打开过程在副本旁边留下的伴生文件。
		_ = os.Remove(tmpPath + "-wal")
		_ = os.Remove(tmpPath + "-shm")
	}
	if _, err := tmp.Write(src); err != nil {
		cleanup()
		return fmt.Errorf("写验证副本: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("写验证副本: %w", err)
	}
	defer cleanup()

	db, err := Open(ctx, Options{Path: tmpPath})
	if err != nil {
		return fmt.Errorf("恢复的账套打不开: %w", err)
	}
	return db.Close()
}
