// Package service 是给界面（Wails / CLI）用的**唯一入口**。
//
// # 为什么要多这一层
//
// 领域层与存储层都刻意做得「窄而正确」：报错具体、不做兜底、
// 不假设调用顺序。这对测试是好事，对界面是负担 —— 界面只想知道
// 「现在能不能结账」「这张资产负债表长什么样」。
//
// service 负责把这堆窄接口拼成界面能直接用的动作：
//
//	打开账套 / 建账 / 首页概览
//	凭证：列表、详情、过账、红字冲销
//	报表：科目余额表、资产负债表、利润表、现金流量表、往来余额表、明细账
//	期末：结账前体检、结账、反结账
//	银行：导入 CSV、匹配、批量过账
//	附件与备份
//	AI：建议、采纳、审计
//
// # 一条铁律
//
// **service 不做任何账务判断。**
// 借贷是否平衡、科目能不能记账、期间能不能过账，全部由领域层回答；
// service 只负责取数、转成界面友好的形状、把错误翻译成人话。
// 一旦 service 里出现「如果借方大于贷方就……」，这套账就不可信了。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"miniaccount/internal/attachment"
	"miniaccount/internal/backup"
	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/ledger"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/domain/report"
	"miniaccount/internal/domain/vat"
	"miniaccount/internal/filelock"
	"miniaccount/internal/store/sqlite"
)

// Service 是账套的操作入口。
type Service struct {
	db       *sqlite.DB
	path     string
	filesDir string
	// occup 是跨进程占用锁（filelock，文档 §2.4）。
	// ":memory:" 账套无文件身份，为空。
	occup *filelock.Lock
	// wlock 是写者锁：仅读写打开持有，挡住第二个读写进程；
	// 只读打开为 nil。
	wlock *filelock.Lock
}

// Options 打开账套的参数。
type Options struct {
	// Path 是 .db 文件路径。传 ":memory:" 可用于演示与测试。
	Path string
	// FilesDir 是附件目录。留空时取 db 同级的 <完整文件名>.files。
	FilesDir string
	// ReadOnly 为真时以只读方式打开（查看备份、只读审计等场景）。
	ReadOnly bool
}

// Open 打开（或创建）一个账套。
//
// 打开前的占用协议（文档 §2.4）分两把锁：
//   - **占用锁（共享）**：所有 opener 都取，挡住恢复中的独占。
//   - **写者锁（独占）**：只有读写打开再取——硬链接/符号链接
//     别名归并到同一文件身份，两个进程经不同路径打开同一
//     账套时，第二个在这里被拒。只读打开不取它，可与一个
//     读写进程共存（WAL 下多读单写安全）。
//
// 新建账套文件尚不存在，先取路径锁占位，文件落位后换身份锁。
func Open(ctx context.Context, opts Options) (*Service, error) {
	var occ, wlock *filelock.Lock
	release := func() {
		if wlock != nil {
			wlock.Release()
			wlock = nil
		}
		if occ != nil {
			occ.Release()
			occ = nil
		}
	}
	pathOnly := false
	// openPath 是 SQLite 实际打开的路径：规约后的真实位置。
	// 经硬链接别名打开一个已被别的进程持有的账套时，占用锁
	// 里的 owner 记录会把它再归并到首个持锁者的路径——WAL
	// 按打开路径字符串命名，不用归并路径就是两份 WAL，
	// 只读端会静默读到过期数据。
	openPath := opts.Path
	if opts.Path != ":memory:" {
		openPath = filelock.CanonicalPath(opts.Path)
		var err error
		if _, statErr := os.Stat(opts.Path); errors.Is(statErr, os.ErrNotExist) {
			id, ierr := filelock.PathIdentity(opts.Path)
			if ierr != nil {
				return nil, ierr
			}
			occ, err = filelock.AcquireIdentity(id, false)
			pathOnly = true
		} else {
			occ, err = filelock.AcquireShared(opts.Path)
			if err == nil {
				if eff := occ.EffectivePath(); eff != "" {
					openPath = eff
				}
			}
		}
		if err != nil {
			if errors.Is(err, filelock.ErrOccupied) {
				return nil, errors.New("账套正在恢复中，请稍后重试")
			}
			return nil, err
		}
		if !opts.ReadOnly {
			var werr error
			wlock, werr = writerLock(openPath, !pathOnly)
			if werr != nil {
				release()
				if errors.Is(werr, filelock.ErrOccupied) {
					return nil, errors.New("账套正被其它进程以读写方式打开")
				}
				return nil, werr
			}
		}
	}
	db, err := sqlite.Open(ctx, sqlite.Options{
		Path: openPath, ReadOnly: opts.ReadOnly,
	})
	if err != nil {
		release()
		return nil, err
	}
	if pathOnly {
		// 文件已落位，换身份锁；换不到就继续持路径锁——
		// 路径锁同样挡得住同路径的 opener 与 restore。
		if lk, err := filelock.AcquireShared(opts.Path); err == nil {
			occ.Release()
			occ = lk
			if eff := lk.EffectivePath(); eff != "" && eff != openPath {
				// 落位间隙里别人用别名登过记：库已按 openPath
				// 打开，重开成登记路径才算同一本账。
				if err2 := db.Close(); err2 == nil {
					db, err = sqlite.Open(ctx, sqlite.Options{
						Path: eff, ReadOnly: opts.ReadOnly})
					if err != nil {
						release()
						return nil, err
					}
					openPath = eff
				}
			}
		}
		if wlock != nil {
			// 写者锁换不到真实身份必须失败，不能降级保留 w-p：
			// 经别名路径并发建账时两边各持不同 w- 键，
			// 「保留占位」等于放行双写。而 occ 的路径锁降级
			// 保留是安全的——open/restore 都过同一把 p- 闸门。
			wl, werr := writerLock(openPath, true)
			if werr != nil {
				_ = db.Close()
				release()
				if errors.Is(werr, filelock.ErrOccupied) {
					return nil, errors.New("账套正被其它进程以读写方式打开")
				}
				return nil, werr
			}
			wlock.Release()
			wlock = wl
		}
	}
	if opts.ReadOnly {
		// 只读打开不跑迁移（迁移要写库），但版本校验不能省：
		// 比程序新的库读出来是错的，比程序旧的会撞「列不存在」。
		if err := db.CheckSchemaVersion(ctx); err != nil {
			_ = db.Close()
			release()
			return nil, err
		}
	} else if err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		release()
		return nil, err
	}
	s := &Service{db: db, path: openPath, filesDir: strings.TrimSpace(opts.FilesDir), occup: occ, wlock: wlock}
	if !opts.ReadOnly {
		s.migrateLegacyFilesDir(ctx)
	}
	return s, nil
}

// writerLock 按文件身份（real=true）或路径身份（文件未落位）
// 取写者锁——独占，第二个读写进程在这里被拒。
func writerLock(path string, real bool) (*filelock.Lock, error) {
	var id filelock.Identity
	var err error
	if real {
		id, err = filelock.IdentityOf(path)
	} else {
		id, err = filelock.PathIdentity(path)
	}
	if err != nil {
		return nil, err
	}
	return filelock.AcquireIdentity(filelock.WriterKey(id), true)
}

// Shutdown 关闭账套，释放数据库连接。
//
// 名字刻意不叫 Close —— 本类型还有一个 Close(ctx, period, by) 是「结账」。
// 两者混在一个名字上，调用点稍不留神就会把「关软件」写成「结账」。
func (s *Service) Shutdown() error {
	err := s.db.Close()
	// 顺序固定：连接彻底关闭后才释放占用锁（文档 §2.4）。
	// 写者锁先放（本进程不再写），占用锁后放。
	if s.wlock != nil {
		s.wlock.Release()
		s.wlock = nil
	}
	if s.occup != nil {
		s.occup.Release()
		s.occup = nil
	}
	return err
}

// DB 暴露底层仓储，供高级用法与测试使用。
//
// 界面代码**不应**直接用它 —— 一旦绕过 service，
// 「界面上看到的」与「service 保证的」就会开始分叉。
func (s *Service) DB() *sqlite.DB { return s.db }

// Path 返回账套文件路径。
func (s *Service) Path() string { return s.path }

// ErrNoBook 表示这个文件还不是一个账套（尚未建账）。
var ErrNoBook = errors.New("账套尚未初始化")

// InvalidInput 标记「调用方传了非法参数」：绑定层把它映射成
// FaultInvalid，MCP 层把它映射成工具的参数错误码，
// 而不是笼统的内部错误。
type InvalidInput struct{ Msg string }

func (e *InvalidInput) Error() string { return e.Msg }

// ---------------------------------------------------------------------------
// 账套
// ---------------------------------------------------------------------------

// CreateBookInput 是建账参数。
type CreateBookInput struct {
	CompanyName string
	CreditCode  string
	LegalPerson string

	// TaxType 是**历史字段**，只用于兼容旧调用方。
	// 新代码请用 VATStatus。
	TaxType string

	// EnterpriseScale 是企业规模类型（micro|small|medium|large）。
	//
	// 所得税与统计口径，依据《中小企业划型标准规定》，看营业收入与
	// 从业人员。允许留空 —— 建账时用户未必想得起来，界面会提示补填。
	EnterpriseScale string
	// VATStatus 是增值税纳税人身份（general|small_scale）。
	//
	// 流转税口径，依据《增值税法》。★ 它与企业规模类型**互不派生**：
	// 小微企业可以自愿登记为一般纳税人；销售额未超 500 万元通常按
	// 小规模纳税，但登记为一般纳税人也是合法的。
	VATStatus string
	// VATStatusEffectiveFrom 是身份生效日（YYYY-MM-DD）；留空取启用日。
	VATStatusEffectiveFrom string

	StartYear  int
	StartMonth int
	// ThroughYear 是预生成期间的年度；为 0 时取启用年度 + 1。
	ThroughYear int
	// CurrentYear/Month 决定哪些期间初始为 open。
	CurrentYear  int
	CurrentMonth int
}

// CreateBook 建账。
//
// 建账是一次性的：科目表、会计期间、账套信息必须同时成立。
// 因此这里先检查是否已建账，避免在一个已有账套上重复预置科目。
func (s *Service) CreateBook(ctx context.Context, in CreateBookInput) (*BookInfo, error) {
	exists, err := s.db.Books().Exists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, sqlite.ErrBookExists
	}
	if _, err := s.db.CreateBook(ctx, sqlite.CreateBookInput{
		CompanyName: in.CompanyName, CreditCode: in.CreditCode,
		LegalPerson: in.LegalPerson, TaxType: in.TaxType,
		EnterpriseScale:        in.EnterpriseScale,
		VATStatus:              in.VATStatus,
		VATStatusEffectiveFrom: in.VATStatusEffectiveFrom,
		StartYear:              in.StartYear, StartMonth: in.StartMonth,
		ThroughYear: in.ThroughYear,
		CurrentYear: in.CurrentYear, CurrentMonth: in.CurrentMonth,
	}); err != nil {
		return nil, err
	}
	// 建账本身是最该留痕的操作之一：它是这本账的起点，
	// 也记下了当时登记的纳税人身份与启用期间。
	s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionBookCreate,
		Summary: "建立账套「" + in.CompanyName + "」",
		Entity:  "book", EntityID: s.path,
		Detail: map[string]any{
			"单位名称": in.CompanyName, "统一社会信用代码": in.CreditCode,
			"法定代表人": in.LegalPerson, "增值税纳税人身份": in.VATStatus,
			"企业规模类型": in.EnterpriseScale,
			"启用期间":   fmt.Sprintf("%04d-%02d", in.StartYear, in.StartMonth),
			"账套文件":   s.path,
		},
	})
	return s.Book(ctx)
}

// BookInfo 是界面上的账套概要。
type BookInfo struct {
	CompanyName string `json:"companyName"`
	CreditCode  string `json:"creditCode"`
	LegalPerson string `json:"legalPerson"`
	TaxType     string `json:"taxType"`
	// TaxTypeLabel 是纳税人身份的**历史**中文名（= VATStatusLabel）。
	//
	// 保留它是为了不改动已有界面的取值路径；新代码请用 VATStatusLabel。
	TaxTypeLabel string `json:"taxTypeLabel"`

	// EnterpriseScale 是企业规模类型；空串表示未填写。
	EnterpriseScale string `json:"enterpriseScale"`
	// EnterpriseScaleLabel 是中文名；未填写时提示补填。
	EnterpriseScaleLabel string `json:"enterpriseScaleLabel"`
	// VATStatus 是增值税纳税人身份。
	VATStatus string `json:"vatStatus"`
	// VATStatusLabel 是纳税人身份的中文名。
	VATStatusLabel string `json:"vatStatusLabel"`
	// VATStatusEffectiveFrom 是当前身份的生效日。
	VATStatusEffectiveFrom string `json:"vatStatusEffectiveFrom"`
	// CanDeductInputVAT 报告当前身份能否抵扣进项税额。
	//
	// ★ 只由**增值税纳税人身份**决定，与企业规模类型无关 ——
	// 界面上要显示这一点，避免用户拿「我是小微企业」去推断能不能抵。
	CanDeductInputVAT bool   `json:"canDeductInputVat"`
	StartPeriod       string `json:"startPeriod"`
	// Periods 是各期间状态，供「账期管理」页面直接渲染。
	Periods []PeriodInfo `json:"periods"`
}

// PeriodInfo 是一个会计期间的状态。
type PeriodInfo struct {
	Year   int    `json:"year"`
	Month  int    `json:"month"`
	Label  string `json:"label"`
	Status string `json:"status"`
	// StatusLabel 是中文状态名。
	StatusLabel string `json:"statusLabel"`
	From        string `json:"from"`
	To          string `json:"to"`
	// VoucherCount 是本期凭证数。
	VoucherCount int `json:"voucherCount"`
	// CanClose / CanReopen 直接告诉界面按钮该不该可点，
	// 免得界面自己去推演「顺序结账」的规则。
	CanClose  bool `json:"canClose"`
	CanReopen bool `json:"canReopen"`
	// Health 仅在需要时填充（结账前体检）。
	Health *HealthInfo `json:"health"`
}

// Book 返回账套概要。
func (s *Service) Book(ctx context.Context) (*BookInfo, error) {
	b, err := s.db.Books().Get(ctx)
	if err != nil {
		// 存储层的三种「还没有账套」表述统一翻译成一个：
		// 界面只关心「要不要引导用户去建账」，不关心底层是查不到行
		// 还是表里没有记录。
		if errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, sqlite.ErrBookNotSetup) {
			return nil, ErrNoBook
		}
		return nil, err
	}
	// ★ 两套身份分别填充，互不派生。
	//
	//	v.Status  —— 增值税纳税人身份（流转税）
	//	v.Scale   —— 企业规模类型（所得税与统计）
	//
	// 界面上必须分开显示：拿「是不是小微企业」去推断「能不能抵扣进项」
	// 是这类软件最常见的算错税的原因。
	vatStatus := vat.VATStatus(b.VATStatus)
	if !vatStatus.Valid() {
		// 老账套或数据被改坏时退回历史字段，而不是给一个空标签
		if b.TaxType == sqlite.TaxTypeSmall {
			vatStatus = vat.VATSmallScale
		} else {
			vatStatus = vat.VATGeneral
		}
	}
	scale := vat.EnterpriseScale(b.EnterpriseScale)
	scaleLabel := scale.Label()
	if !scale.Valid() {
		// 未填写：显示成「未填写」并提示补填，而不是猜一个。
		// 猜错会让小型微利企业优惠算错。
		scaleLabel = "未填写"
	}

	info := &BookInfo{
		CompanyName: b.CompanyName, CreditCode: b.CreditCode,
		LegalPerson: b.LegalPerson,
		TaxType:     b.TaxType,
		StartPeriod: fmt.Sprintf("%d-%02d", b.StartYear, b.StartMonth),

		EnterpriseScale:        b.EnterpriseScale,
		EnterpriseScaleLabel:   scaleLabel,
		VATStatus:              string(vatStatus),
		VATStatusLabel:         vatStatus.Label(),
		VATStatusEffectiveFrom: b.VATStatusEffectiveFrom,
		CanDeductInputVAT:      vatStatus.CanDeductInput(),
	}
	// 历史字段保持与 vat_status 一致，免得旧界面读不到
	info.TaxTypeLabel = info.VATStatusLabel

	cal, err := s.db.Periods().Load(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.voucherCountsByPeriod(ctx)
	if err != nil {
		return nil, err
	}

	// 顺序结账：只有「前面全部已结账」的期间才能结，
	// 只有「后面全部未结账」的已结账期间才能反结。
	// 这两个判断放在这里一次算清，界面只管照画。
	priorAllClosed := true
	for _, k := range cal.Keys() {
		p, ok := cal.Get(k.Year, k.Month)
		if !ok {
			continue
		}
		info.Periods = append(info.Periods, PeriodInfo{
			Year: k.Year, Month: k.Month, Label: k.String(),
			Status: string(p.Status), StatusLabel: p.Status.Label(),
			From: p.Range.From.String(), To: p.Range.To.String(),
			VoucherCount: counts[k],
			CanClose:     p.Status == period.StatusOpen && priorAllClosed,
		})
		if p.Status != period.StatusClosed {
			priorAllClosed = false
		}
	}
	// 反结账：从最后一个已结账期间往前找，只有「后面没有已结账期间」的才能反结
	lastClosed := -1
	for i, p := range info.Periods {
		if p.Status == string(period.StatusClosed) {
			lastClosed = i
		}
	}
	if lastClosed >= 0 {
		info.Periods[lastClosed].CanReopen = true
	}
	return info, nil
}

func (s *Service) voucherCountsByPeriod(ctx context.Context) (map[period.Key]int, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT year, month, COUNT(*) FROM voucher WHERE status <> 'voided'
		 GROUP BY year, month`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := map[period.Key]int{}
	for rows.Next() {
		var y, m, n int
		if err := rows.Scan(&y, &m, &n); err != nil {
			return nil, err
		}
		out[period.NewKey(y, m)] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 首页概览
// ---------------------------------------------------------------------------

// Dashboard 是首页要展示的全部数字。
//
// 一次查完而不是让界面发十几次请求：单连接 SQLite 下，
// 十几次来回的代价是串行等待，而不是并发加速。
type Dashboard struct {
	Book *BookInfo `json:"book"`

	// CurrentPeriod 是当前可记账期间（最早的 open 期间）。
	CurrentPeriod string `json:"currentPeriod"`
	// LatestClosedPeriod 是最近一个已结账期间。
	LatestClosedPeriod string `json:"latestClosedPeriod"`

	// Assets / Liabilities / Equity 是最新一期的资产负债概要。
	Assets      money.Money `json:"assets"`
	Liabilities money.Money `json:"liabilities"`
	Equity      money.Money `json:"equity"`

	// PeriodIncome / PeriodExpense / PeriodProfit 是本期损益。
	PeriodIncome  money.Money `json:"periodIncome"`
	PeriodExpense money.Money `json:"periodExpense"`
	PeriodProfit  money.Money `json:"periodProfit"`

	// ClosingCash 是期末现金及现金等价物。
	ClosingCash money.Money `json:"closingCash"`

	// DraftVouchers 是待过账的草稿数 —— 首页要提醒用户处理。
	DraftVouchers int `json:"draftVouchers"`
	// UnpostedBankFlows 是尚未生成凭证的银行流水数。
	UnpostedBankFlows int `json:"unpostedBankFlows"`
	// MissingAttachments 是有凭证但没附件的情况数（提示级）。
	MissingAttachments int `json:"missingAttachments"`

	// BalanceSheetIssues 是资产负债表的勾稽问题。
	//
	// 首页**必须**把它们带出来：一张勾稽不成立的资产负债表
	// 看着数字齐全，实际上不能用。只在结账时才报出来太晚了。
	BalanceSheetIssues []string `json:"balanceSheetIssues"`
}

// 报表项目的官方行次，用于从报表定义里取值。
const (
	bsLineAssets      = 30 // 资产总计
	bsLineLiabilities = 47 // 负债合计
	bsLineEquity      = 52 // 所有者权益合计
	plLineIncome      = 1  // 营业收入
	plLineProfit      = 30 // 利润总额
)

// Overview 生成首页概览。
func (s *Service) Overview(ctx context.Context) (*Dashboard, error) {
	book, err := s.Book(ctx)
	if err != nil {
		return nil, err
	}
	// ★ BalanceSheetIssues 预置成空切片：nil 会被编成 JSON 的 null，
	// 而界面拿它当数组用。空账套没有勾稽问题 → 这一项恰好是 nil →
	// 首页一打开就崩。这类字段只有「没有数据时」才炸，最难在开发时发现。
	d := &Dashboard{Book: book, BalanceSheetIssues: []string{}}
	for i := len(book.Periods) - 1; i >= 0; i-- {
		p := book.Periods[i]
		if p.Status == string(period.StatusClosed) && d.LatestClosedPeriod == "" {
			d.LatestClosedPeriod = p.Label
		}
		if p.Status == string(period.StatusOpen) {
			d.CurrentPeriod = p.Label
		}
	}

	// 取「当前可记账期间」而不是「账套最后一个期间」。
	//
	// 后者是预生成的未来期间（可能还没启用），拿它去算损溢
	// 永远得到一张空表 —— 界面上看起来就像「这个月什么都没发生」。
	// 当前期间在 Book() 里已经算好（最早的 open 期间），直接用。
	k := period.NewKey(0, 0)
	for _, p := range book.Periods {
		if p.Status == string(period.StatusOpen) {
			k = period.NewKey(p.Year, p.Month)
			break
		}
	}
	if !k.Valid() {
		// 全部期间都已结账：退回到最后一个期间
		if len(book.Periods) > 0 {
			last := book.Periods[len(book.Periods)-1]
			k = period.NewKey(last.Year, last.Month)
		} else {
			now := calendar.Today()
			k = period.NewKey(now.Year, now.Month)
		}
	}
	asOf := periodEnd(k)

	bs, _, issues, err := s.db.Reports().BuildBalanceSheet(ctx, asOf)
	if err != nil {
		return nil, err
	}
	// 勾稽问题由结账前体检负责报出，首页只展示数字，
	// 但**要把问题带出去**让界面能挂一个警示角标
	for _, is := range issues {
		if is.Fatal {
			d.BalanceSheetIssues = append(d.BalanceSheetIssues, is.String())
		}
	}
	d.Assets = lineValue(bs, bsLineAssets)
	d.Liabilities = lineValue(bs, bsLineLiabilities)
	d.Equity = lineValue(bs, bsLineEquity)

	pl, _, _, err := s.db.Reports().BuildIncomeStatement(ctx, k)
	if err != nil {
		return nil, err
	}
	for _, no := range []int{plLineIncome, plLineProfit} {
		if _, ok := pl.Line(no); !ok {
			return nil, fmt.Errorf("service: 利润表定义缺少行次 %d", no)
		}
	}
	d.PeriodIncome = lineValue(pl, plLineIncome)
	d.PeriodProfit = lineValue(pl, plLineProfit)
	// ★ 费用 = 收入 − 利润总额。
	//
	// 利润表里没有「费用合计」这一行（原来误取了行14「管理费用」
	// 一个单行，本期利润被系统性高估）。费用侧本来就是
	// 收入、投资收益、营业外收支的差值 —— 用利润总额倒推
	// 既与官方勾稽关系一致，又始终自洽：利润 = 收入 − 费用。
	d.PeriodExpense = d.PeriodIncome.Sub(d.PeriodProfit)

	if st, err := s.db.CashFlow().StatementForPeriod(ctx, k, nil); err == nil {
		d.ClosingCash = st.ClosingCash
	}

	if err := s.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM voucher WHERE status = 'draft'`).Scan(&d.DraftVouchers); err != nil {
		return nil, translate(err)
	}
	if err := s.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bank_flow WHERE voucher_id IS NULL AND status <> 'ignored'`).
		Scan(&d.UnpostedBankFlows); err != nil {
		return nil, translate(err)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// 期末
// ---------------------------------------------------------------------------

// HealthItemInfo 是体检报告中的一项。
type HealthItemInfo struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Level  string `json:"level"` // ok | warn | error
	Detail string `json:"detail"`
	Count  int    `json:"count"`
}

// HealthInfo 是结账前体检报告。
type HealthInfo struct {
	Period   string           `json:"period"`
	CanClose bool             `json:"canClose"`
	Summary  string           `json:"summary"`
	Items    []HealthItemInfo `json:"items"`
	Errors   []HealthItemInfo `json:"errors"`
	Warnings []HealthItemInfo `json:"warnings"`
}

// CheckHealth 执行结账前体检。
func (s *Service) CheckHealth(ctx context.Context, k period.Key) (*HealthInfo, error) {
	h, err := s.db.CheckPeriodHealth(ctx, k)
	if err != nil {
		return nil, err
	}
	return convertHealth(h), nil
}

func convertHealth(h *sqlite.PeriodHealth) *HealthInfo {
	out := &HealthInfo{
		Period: h.Period.String(), CanClose: h.CanClose(), Summary: h.Summary(),
	}
	conv := func(items []sqlite.HealthItem) []HealthItemInfo {
		res := make([]HealthItemInfo, 0, len(items))
		for _, it := range items {
			res = append(res, HealthItemInfo{
				Key: it.Key, Title: it.Title, Level: string(it.Level),
				Detail: it.Detail, Count: it.Count,
			})
		}
		return res
	}
	conv2 := func(items []sqlite.HealthItem) []HealthItemInfo {
		res := make([]HealthItemInfo, 0, len(items))
		for _, it := range items {
			res = append(res, HealthItemInfo{
				Key: it.Key, Title: it.Title, Level: string(it.Level),
				Detail: it.Detail, Count: it.Count,
			})
		}
		return res
	}
	out.Items = conv(h.Items)
	out.Errors = conv2(h.Errors())
	out.Warnings = conv2(h.Warnings())
	return out
}

// ClosingPreview 是结账预览。
type ClosingPreview struct {
	Period string `json:"period"`
	// Year / Month 是这个预览针对的期间。
	//
	// ★ 补这两个字段是拿一次事故换来的：界面原来从预览对象上取
	// `preview.year`，而它不存在 —— 取到 undefined，JSON 又把
	// undefined 的键丢掉，Go 侧收到零值，结账直接报
	// 「会计期间 0000-00 非法」，且结账/反结账**完全不可用**。
	//
	// 界面那边已经改成自己记住选中的期间（不该指望预览顺带带着它），
	// 这里补上是因为「这个预览是关于哪个期间的」本来就该由它自己说清楚。
	Year  int `json:"year"`
	Month int `json:"month"`
	// Steps 是结账会做的事，供界面逐步展示。
	Steps []ClosingStep `json:"steps"`
	// Income / Expense / Profit 是本期的损益。
	Income  money.Money `json:"income"`
	Expense money.Money `json:"expense"`
	Profit  money.Money `json:"profit"`
	// Entries 是将会写入的结转分录。
	Entries []ClosingEntry `json:"entries"`
	// DraftCount 是本期待过账的草稿凭证张数。
	//
	// ★ 账套里唯一的过账时机是结账，所以这张数就是「点确认之后
	// 账上会多出几张凭证」。不说清楚的话，用户点完结账才发现
	// 账上凭空多了一批分录。
	DraftCount int `json:"draftCount"`
	// DraftSamples 是其中前几张的日期与摘要，供界面展示。
	DraftSamples []string `json:"draftSamples"`
	// Health 是体检报告。
	Health *HealthInfo `json:"health"`
}

// ClosingStep 是结账流程的一步。
type ClosingStep struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Done    bool   `json:"done"`
	Skipped bool   `json:"skipped"`
}

// ClosingEntry 是一条拟写入的结转分录。
type ClosingEntry struct {
	AccountCode string      `json:"accountCode"`
	Summary     string      `json:"summary"`
	Debit       money.Money `json:"debit"`
	Credit      money.Money `json:"credit"`
	// AuxDesc 是辅助核算的中文描述，便于界面展示。
	AuxDesc string `json:"auxDesc"`
}

// PreviewClose 预览结账结果，**不写任何数据**。
func (s *Service) PreviewClose(ctx context.Context, k period.Key) (*ClosingPreview, error) {
	h, err := s.db.CheckPeriodHealth(ctx, k)
	if err != nil {
		return nil, err
	}
	// ★ 预览必须和真实结账看到同一套账。
	//
	// 真实结账会先把本期草稿全部过账、再按总账算结转；
	// 直接拿已过账总账算出来的预览，有草稿时就和实际结果
	// 对不上（200 元收入草稿：预览收入为零，结账却生成了
	// 两条结转分录）。DryRunClose 在写事务里重放全部步骤后
	// 回滚，预览与实结天然同源。
	//
	// postingBy 用占位值：dry-run 全部回滚，制单人不会落库。
	res, err := s.db.Closing().DryRunClose(ctx, sqlite.CloseInput{
		Period: k, PostingBy: "预览", At: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	plan := res.Plan
	out := &ClosingPreview{
		Period: k.String(), Year: k.Year, Month: k.Month,
		Health: convertHealth(h),
	}
	for _, st := range plan.Steps {
		out.Steps = append(out.Steps, ClosingStep{
			Key: st.Key, Title: st.Title, Detail: st.Detail,
			Done: st.Done, Skipped: st.Skipped,
		})
	}
	if plan.Closing != nil {
		out.Income = plan.Closing.TotalIncome
		out.Expense = plan.Closing.TotalExpense
		out.Profit = plan.Closing.Profit
	}
	for _, e := range plan.ClosingEntries {
		out.Entries = append(out.Entries, ClosingEntry{
			AccountCode: e.AccountCode, Summary: e.Summary,
			Debit: e.Debit, Credit: e.Credit, AuxDesc: auxDesc(e.Aux),
		})
	}
	n, samples, err := s.db.Vouchers().DraftsOfPeriod(ctx, k)
	if err != nil {
		return nil, err
	}
	out.DraftCount = n
	out.DraftSamples = nonNilSlice(samples)
	return out, nil
}

// CloseResult 是结账结果。
type CloseResult struct {
	Period    string `json:"period"`
	VoucherNo string `json:"voucherNo"`
	VoucherID int64  `json:"voucherId"`
	// VoucherCreated 为假表示本期无损益可转，只关期间。
	VoucherCreated bool        `json:"voucherCreated"`
	Summary        string      `json:"summary"`
	Health         *HealthInfo `json:"health"`
	// PostedDrafts 是本次结账顺带过账的草稿张数。
	//
	// ★ 凭证录完只落草稿，过账只发生在结账。这个数要报给用户 ——
	// 「结账成功了」和「结账成功了，顺便把 12 张草稿记进了账」
	// 是两件事，后者用户必须知道。
	PostedDrafts int `json:"postedDrafts"`
	// PostedNos 是本次过账分配到的凭证号。
	PostedNos []string `json:"postedNos"`
}

// Close 执行结账。
func (s *Service) Close(ctx context.Context, k period.Key, postingBy string) (*CloseResult, error) {
	res, err := s.db.Closing().ClosePeriod(ctx, sqlite.CloseInput{
		Period: k, PostingBy: postingBy, At: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	out := &CloseResult{
		Period: k.String(), VoucherNo: res.VoucherNo,
		VoucherID: res.VoucherID, VoucherCreated: res.Planned(),
		Summary:   res.Plan.Summary(),
		PostedNos: nonNilSlice(res.PostedNos()),
	}
	if res.PostedDrafts != nil {
		out.PostedDrafts = res.PostedDrafts.Posted
	}
	if res.Health != nil {
		out.Health = convertHealth(res.Health)
	}
	// ★ 规范点名要记「会计期间的打开、关闭」；重开已结账期间还要
	// 记**期间的起止日期** —— 这里把整月的起止都写进明细。
	s.recordAudit(ctx, AuditEvent{
		Action: audit.ActionPeriodClose, Summary: "结账 " + k.String(),
		Entity: "period", EntityID: k.String(), Operator: postingBy,
		Detail: map[string]any{
			"期间":   k.String(),
			"起":    periodStart(k).String(),
			"止":    periodEnd(k).String(),
			"结转凭证": res.VoucherNo,
			"说明":   res.Plan.Summary(),
		},
	})
	return out, nil
}

// periodStart 给出期间的第一天。
func periodStart(k period.Key) calendar.Date {
	d, err := calendar.New(k.Year, k.Month, 1)
	if err != nil {
		return calendar.Today()
	}
	return d
}

// ReopenResult 是反结账结果。
type ReopenResult struct {
	Period string `json:"period"`
	// Reversed 是被红字冲销的结转凭证号。
	Reversed   []string `json:"reversed"`
	VoucherIDs []int64  `json:"voucherIds"`
}

// Reopen 执行反结账。
func (s *Service) Reopen(ctx context.Context, k period.Key, postingBy string) (*ReopenResult, error) {
	res, err := s.db.Closing().ReopenPeriod(ctx, sqlite.ReopenInput{
		Period: k, PostingBy: postingBy, At: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	s.recordAudit(ctx, AuditEvent{
		Action: audit.ActionPeriodReopen, Summary: "反结账（重新开启）" + k.String(),
		Entity: "period", EntityID: k.String(), Operator: postingBy,
		Detail: map[string]any{
			"期间":      k.String(),
			"起":       periodStart(k).String(),
			"止":       periodEnd(k).String(),
			"冲销的结转凭证": res.Reversed,
		},
	})
	return &ReopenResult{
		Period: k.String(), Reversed: res.Reversed, VoucherIDs: res.VoucherIDs,
	}, nil
}

// ---------------------------------------------------------------------------
// AI
// ---------------------------------------------------------------------------
// 附件与备份
// ---------------------------------------------------------------------------

// FilesDir 返回附件目录。
//
// ★ 按账套隔离，取 db 同级的 <完整文件名>.files。
//
// 曾经是 <db目录>/.files —— 同目录下的所有账套共用一个附件目录；
// 后来改成剥扩展名的 <stem>.files —— same.db 与 same.sqlite 又撞回
// 同一个目录，互相列孤儿、互相打进备份。用完整文件名派生才没有
// 歧义：same.db → same.db.files，same.sqlite → same.sqlite.files。
func (s *Service) FilesDir() string {
	if s.filesDir != "" {
		return s.filesDir
	}
	base := s.path
	if base == ":memory:" || base == "" {
		return filepath.Join(".", ".files")
	}
	return base + ".files"
}

// filesDirFor 按与 FilesDir 相同的规则给任意 db 路径推导附件目录。
func filesDirFor(dbPath string) string { return dbPath + ".files" }

// migrateLegacyFilesDir 把旧布局附件目录里、本账套引用的附件复制到
// 按账套隔离的新目录。历史布局有两代：
//   - <db目录>/.files（最初的全局共享目录）
//   - <stem>.files（剥扩展名，same.db 与 same.sqlite 撞名那代）
//
// 只复制、不移动：内容寻址意味着同一 hash 文件可能被同目录下其他
// 账套引用，移走了别人就丢。单文件失败不阻断打开 —— 文件还留在旧
// 目录里，下次启动会再试；让建不了目录这种错误只影响附件写入
// （届时附件操作自己会报清楚的错），不该让整本账打不开。
func (s *Service) migrateLegacyFilesDir(ctx context.Context) {
	if s.path == ":memory:" || s.path == "" || s.filesDir != "" {
		return
	}
	newDir := s.FilesDir()
	var legacyDirs []string
	for _, d := range s.legacyDirs() {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			legacyDirs = append(legacyDirs, d)
		}
	}
	if len(legacyDirs) == 0 {
		return
	}
	// 按表存在性分别查：attachment_v2 时代的账套可能缺
	// audit_evidence，UNION 整句失败反而什么都不迁。
	var hashes []string
	for _, table := range []string{"attachment", "audit_evidence"} {
		var one int
		err := s.db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`,
			table).Scan(&one)
		if err != nil {
			// 查表失败 ≠ 表不存在 —— 静默跳过会把这张表的
			// 附件整个漏迁，至少要留下可查的痕迹。
			_, _ = os.Stderr.WriteString("[files] 附件表检查失败，旧附件未迁移: " + err.Error() + "\n")
			return
		}
		if one == 0 {
			continue
		}
		rows, err := s.db.SQL().QueryContext(ctx,
			`SELECT sha256 FROM `+table)
		if err != nil {
			// 查不出清单就无从迁移 —— 静默返回会让老附件「看起来
			// 凭空消失」，至少要留下可查的痕迹。
			_, _ = os.Stderr.WriteString("[files] 附件清单读取失败，旧附件未迁移: " + err.Error() + "\n")
			return
		}
		for rows.Next() {
			var h string
			if rows.Scan(&h) == nil {
				hashes = append(hashes, h)
			}
		}
		rows.Close()
	}
	for _, h := range hashes {
		if len(h) < 4 {
			continue
		}
		dst := filepath.Join(newDir, h[:2], h)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		// ★ 逐目录找「内容对得上文件名」的副本：两个旧目录
		// 都可能有同名文件，先读到的若是坏副本，迁过去的就是
		// 一份读不掉的坏数据 —— 取首个 hash 校验通过的。
		var data []byte
		for _, legacy := range legacyDirs {
			d, err := os.ReadFile(filepath.Join(legacy, h[:2], h))
			if err != nil {
				continue
			}
			sum := sha256.Sum256(d)
			if hex.EncodeToString(sum[:]) == h {
				data = d
				break
			}
		}
		if data == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			_, _ = os.Stderr.WriteString("[files] 附件目录迁移失败: " + err.Error() + "\n")
			return
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			_, _ = os.Stderr.WriteString("[files] 附件迁移失败: " + err.Error() + "\n")
			return
		}
	}
}

// Attachments 返回附件仓库。
//
// 附件按 sha256 内容寻址存放在账套目录下的 .files/，
// 不入库：附件以 PDF 为主，入库会让单个账套文件迅速膨胀到 GB 级，
// 而备份与恢复也都因此变得笨重。
func (s *Service) Attachments() (*attachment.Store, error) {
	return attachment.New(attachment.Options{
		Dir: s.FilesDir(), FallbackDirs: s.legacyDirs(),
	})
}

// legacyDirs 返回本账套旧布局的附件目录（共享 .files 与
// <stem>.files 两代），不含当前目录本身、不要求存在。
//
// 只读打开不做物理迁移、迁移也可能只迁了一半 —— 读附件
// 与备份收集都要把这些目录算进去，才不会把完好的原件
// 误报「缺失」。
func (s *Service) legacyDirs() []string {
	if s.path == ":memory:" || s.path == "" || s.filesDir != "" {
		return nil
	}
	newDir := s.FilesDir()
	stem := strings.TrimSuffix(filepath.Base(s.path), filepath.Ext(s.path))
	var out []string
	for _, d := range []string{
		filepath.Join(filepath.Dir(s.path), ".files"),
		filepath.Join(filepath.Dir(s.path), stem+".files"),
	} {
		if d != newDir {
			out = append(out, d)
		}
	}
	return out
}

// BackupOptions 是备份参数。
type BackupOptions struct {
	Dest string
	// IncludeFiles 为假时只备份数据库（用于「快速备份」）。
	IncludeFiles bool
}

// Backup 打包备份为一个 .mabak 文件。
func (s *Service) Backup(ctx context.Context, opts BackupOptions) (*backup.Manifest, error) {
	bk := &backup.Backup{
		DBPath: s.path, FilesDir: s.FilesDir(), AppVersion: AppVersion,
		FallbackFilesDirs: s.legacyDirs(),
	}
	if info, err := s.Book(ctx); err == nil {
		bk.CompanyName, bk.CreditCode = info.CompanyName, info.CreditCode
	}
	return bk.Create(ctx, opts.Dest, backup.Options{
		IncludeFiles: opts.IncludeFiles,
	})
}

// RestoreOptions 是恢复参数。
type RestoreOptions struct {
	Archive string
	DBPath  string
	// FilesDir 是附件目录；留空时按规约后的 DBPath 推导
	// （<完整文件名>.files，与 FilesDir() 同规则）。
	// 恢复到符号链接时若由调用方按链接名推导，DB 会落在
	// 真实路径而附件落进链接名目录 —— 两侧对不上。
	FilesDir string
}

// Restore 从备份恢复。
//
// 新库落位后立即用 VerifyOpenable 验「能打开」——版本过新或
// 损坏都在 Restore 的回滚守卫内触发，原账套自动放回，
// 而不是留给打开阶段再失败、让用户手工翻暂存目录。
func (s *Service) Restore(ctx context.Context, o RestoreOptions) (*backup.RestoreResult, error) {
	// 目标路径先规约：往一个「指向账套的符号链接」上恢复，
	// os.Rename 会把链接本身换成独立文件，原账套原地不动
	// ——命令报告成功，实际一字没恢复。规约到真实位置再动。
	dbPath := filelock.CanonicalPath(o.DBPath)
	// 恢复全程持独占占用（§2.4）：任何进程持共享打开着就拒绝，
	// 而不是让它对着已被替换的文件继续写。
	var lk *filelock.Lock
	var err error
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		var id filelock.Identity
		if id, err = filelock.PathIdentity(dbPath); err == nil {
			lk, err = filelock.AcquireIdentity(id, true)
		}
	} else {
		lk, err = filelock.AcquireExclusive(dbPath)
	}
	if err != nil {
		if errors.Is(err, filelock.ErrOccupied) {
			return nil, errors.New("账套正被使用（界面/命令行/MCP 服务），请先断开再恢复")
		}
		return nil, err
	}
	defer lk.Release()
	filesDir := strings.TrimSpace(o.FilesDir)
	if filesDir == "" {
		filesDir = filesDirFor(dbPath)
	}
	return backup.Restore(ctx, o.Archive, dbPath, filesDir,
		sqlite.VerifyOpenable)
}

// InspectBackup 只读查看备份内容，用于「恢复前确认」。
func (s *Service) InspectBackup(path string) (*backup.Manifest, error) {
	return backup.Inspect(path)
}

// AppVersion 是写进备份清单的版本号。
//
// 恢复时用它判断兼容性：低版本备份恢复到高版本程序是安全的
// （迁移只向前），反过来不行。
var AppVersion = "0.2.5"

// BuildStamp 是构建时间，由构建脚本用 -ldflags 注入。
//
// 留空是正常的（`go build` 直接编就没有）。它的用处是：
// 同一版本号下改了东西重新构建时，能分辨手上这份是哪一次编的。
var BuildStamp = ""

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// lineValue 取报表某行的金额；行次不存在时返回 0。
//
// 行次不存在意味着定义文件被改坏了 —— 这里选择返回 0 并让
// 调用方在关键行上显式检查，而不是 panic：
// 一张报表少一行不该让整个程序崩掉。
func lineValue(d *report.Definition, no int) money.Money {
	if d == nil {
		return 0
	}
	l, ok := d.Line(no)
	if !ok {
		return 0
	}
	return l.Value
}

// translate 把存储层错误翻译成界面能识别的话。
func translate(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sqlite.ErrNotFound):
		return fmt.Errorf("记录不存在：%w", err)
	default:
		return err
	}
}

// periodEnd 返回某会计期间的最后一天。

// periodEnd 返回某会计期间的最后一天。
func periodEnd(k period.Key) calendar.Date {
	d, err := calendar.New(k.Year, k.Month, calendar.DaysInMonth(k.Year, k.Month))
	if err != nil {
		return calendar.Today()
	}
	return d
}

func mustDate(s string) calendar.Date {
	d, err := calendar.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("service: 内部日期 %q 非法: %v", s, err))
	}
	return d
}

// auxDesc 把辅助核算渲染成中文描述。
func auxDesc(a ledger.Aux) string {
	var parts []string
	add := func(label string, id *int64) {
		if id != nil {
			parts = append(parts, fmt.Sprintf("%s#%d", label, *id))
		}
	}
	add("往来", a.ContactID)
	add("员工", a.EmployeeID)
	add("部门", a.DeptID)
	add("项目", a.ProjectID)
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// ★ 见 desktop/guard.go 里同名函数的说明：Go 编 JSON 时 nil 切片是
// `null`，而界面把列表字段当数组用（`data.rows.length`），拿到 null
// 直接抛异常。这个差别**只在没有数据时出现** —— 开发时账套里都是
// 演示数据，怎么点都不崩；用户新建一个空账套点进去立刻炸。
//
// 放在 JSON 边界上把 nil 一律换掉，比让每个界面各写一遍 `?? []` 可靠。
func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
