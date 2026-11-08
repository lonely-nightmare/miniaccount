package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"miniaccount/internal/domain/account"
	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/calendar"
	"miniaccount/internal/domain/ledger"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/period"
	"miniaccount/internal/domain/voucher"
	"miniaccount/internal/store/sqlite"
)

// 凭证相关错误。
var (
	// ErrVoucherLocked 表示目标凭证当前状态不允许该操作。
	ErrVoucherLocked = errors.New("凭证当前状态不允许该操作")
)

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

// VoucherQuery 是凭证列表的查询条件。
type VoucherQuery struct {
	Year, Month int
	Status      string
	Keyword     string
	Limit       int
	// Offset / AccountPrefix 只对分页接口 VouchersPage 生效；
	// GUI 的整月列表 Vouchers 不用它们。
	Offset        int
	AccountPrefix string
}

// VoucherSummary 是列表里的一张凭证。
//
// 刻意**不带分录**：一个月的凭证可能有几百张，
// 列表只需要「号、日期、摘要、金额、状态」，把分录一起查出来
// 会让一次列表请求变成几百次关联查询（单连接 SQLite 下是几百次串行等待）。
type VoucherSummary struct {
	ID     int64  `json:"id"`
	No     string `json:"no"`
	Word   string `json:"word"`
	Date   string `json:"date"`
	Remark string `json:"remark"`
	Status string `json:"status"`
	// StatusLabel 是中文状态名。
	StatusLabel string `json:"statusLabel"`
	// AttachCount 是附单据数。
	AttachCount int `json:"attachCount"`
	// Amount 是借方合计（分）。
	Amount money.Money `json:"amount"`
	// Source 是来源，用于界面显示「银行流水」「工资」等角标。
	Source string `json:"source"`
	// SourceLabel 是来源中文名。
	SourceLabel string `json:"sourceLabel"`
	// CreatedByAI 标记这张凭证的初稿由 AI 提议生成。
	CreatedByAI bool `json:"createdByAi"`
	// CreatedBy / PostedBy 是制单人与记账人签章。
	CreatedBy string `json:"createdBy"`
	PostedBy  string `json:"postedBy"`
	// Lines 是分录行数。列表上不显示分录，但行数能帮人一眼判断是否完整。
	Lines int `json:"lines"`
	// UpdatedAt 是乐观锁戳，删除时回传。
	UpdatedAt string `json:"updatedAt"`
}

// Vouchers 返回凭证列表。
func (s *Service) Vouchers(ctx context.Context, q VoucherQuery) ([]VoucherSummary, error) {
	s.recordView(ctx, AuditEvent{
		Action: audit.ActionViewVouchers, Entity: "voucher",
		EntityID: fmt.Sprintf("%04d-%02d/%s", q.Year, q.Month, q.Status),
		Summary:  fmt.Sprintf("查看凭证列表 %04d-%02d", q.Year, q.Month),
		Detail:   map[string]any{"期间": fmt.Sprintf("%04d-%02d", q.Year, q.Month), "状态": q.Status, "关键字": q.Keyword},
	})
	// Limit 封顶：界面传什么我们都兜个底 —— 整库凭证一把
	// 全拉回前端，大账套会把 WebView 卡死。
	limit := q.Limit
	if limit > 2000 {
		limit = 2000
	}
	rows, err := s.db.Vouchers().List(ctx, sqlite.ListFilter{
		Year: q.Year, Month: q.Month, Status: q.Status,
		Keyword: q.Keyword, Limit: limit,
	})
	if err != nil {
		return nil, err
	}

	// 凭证头查询不加载分录；列表金额和行数必须从分录聚合，不能调用 v.Amount()。
	stats, err := s.periodLineStats(ctx, q.Year, q.Month)
	if err != nil {
		return nil, err
	}

	out := make([]VoucherSummary, 0, len(rows))
	for _, v := range rows {
		out = append(out, VoucherSummary{
			ID: v.ID, No: v.No, Word: string(v.Word), Date: v.BizDate.String(),
			Remark: v.Remark, Status: string(v.Status),
			StatusLabel: v.Status.Label(), AttachCount: v.AttachCount,
			Amount: stats[v.ID].sum, Source: string(v.Source),
			SourceLabel: v.Source.Label(), CreatedByAI: v.CreatedByAI,
			CreatedBy: v.CreatedBy, PostedBy: v.PostedBy,
			Lines: stats[v.ID].count, UpdatedAt: v.UpdatedAtText,
		})
	}
	return out, nil
}

// VoucherDetail 是一张凭证的完整内容。
type VoucherDetail struct {
	ID     int64  `json:"id"`
	No     string `json:"no"`
	Word   string `json:"word"`
	Date   string `json:"date"`
	Remark string `json:"remark"`
	Status string `json:"status"`
	// StatusLabel 是中文状态名。
	StatusLabel string `json:"statusLabel"`
	AttachCount int    `json:"attachCount"`
	Source      string `json:"source"`
	SourceLabel string `json:"sourceLabel"`
	CreatedByAI bool   `json:"createdByAi"`

	CreatedBy  string `json:"createdBy"`
	ReviewedBy string `json:"reviewedBy"`
	PostedBy   string `json:"postedBy"`
	PostedAt   string `json:"postedAt"`

	// ReversesNo 是被本凭证冲销的原凭证号（红字凭证才有）。
	ReversesNo string `json:"reversesNo"`
	// VoidedByNo 是冲销本凭证的那张红字凭证号。
	VoidedByNo string `json:"voidedByNo"`

	Lines []VoucherLine `json:"lines"`

	// TotalDebit / TotalCredit 用于界面上的「借贷平衡」提示。
	TotalDebit  money.Money `json:"totalDebit"`
	TotalCredit money.Money `json:"totalCredit"`
	// Balanced 为假时界面必须显眼提示。
	Balanced bool `json:"balanced"`
	// UpdatedAt 是乐观锁戳（updated_at 存储原文）：改/删这张
	// 凭证时必须把它回传（expectedUpdatedAt）。
	UpdatedAt string `json:"updatedAt"`

	// CanEdit / CanDelete / CanPost / CanReverse 由服务层算好，
	// 界面照着禁用按钮即可 —— 状态机的规则只在一处实现。
	CanEdit    bool `json:"canEdit"`
	CanDelete  bool `json:"canDelete"`
	CanPost    bool `json:"canPost"`
	CanReverse bool `json:"canReverse"`
	// BlockedReason 说明为什么不能操作（为空表示都可以）。
	BlockedReason string `json:"blockedReason"`
}

// VoucherLine 是凭证的一条分录。
type VoucherLine struct {
	LineNo      int         `json:"lineNo"`
	AccountCode string      `json:"accountCode"`
	AccountName string      `json:"accountName"`
	Summary     string      `json:"summary"`
	Debit       money.Money `json:"debit"`
	Credit      money.Money `json:"credit"`
	// AuxDesc 是辅助核算的中文描述。
	AuxDesc string `json:"auxDesc"`
	// 辅助核算的实体 id。★ 编辑器载入草稿要靠它们回填：
	// 只有 AuxDesc 文本的话，打开编辑器四维全是 null，
	// 保存回去就把辅助核算静默清空了 —— 而界面上往来单位名
	// 照常显示，用户根本不知道自己丢了数据。
	ContactID  *int64 `json:"contactId"`
	EmployeeID *int64 `json:"employeeId"`
	DeptID     *int64 `json:"deptId"`
	ProjectID  *int64 `json:"projectId"`
}

// Voucher 返回一张凭证的完整内容。
func (s *Service) Voucher(ctx context.Context, id int64) (*VoucherDetail, error) {
	v, err := s.db.Vouchers().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	tree, err := s.db.Accounts().Tree(ctx)
	if err != nil {
		return nil, err
	}
	names, err := s.contactNames(ctx)
	if err != nil {
		return nil, err
	}
	deptNames, err := s.departmentNames(ctx)
	if err != nil {
		return nil, err
	}

	d := &VoucherDetail{
		ID: v.ID, No: v.No, Word: string(v.Word), Date: v.BizDate.String(),
		UpdatedAt: v.UpdatedAtText,
		Remark:    v.Remark, Status: string(v.Status),
		StatusLabel: v.Status.Label(), AttachCount: v.AttachCount,
		Source: string(v.Source), SourceLabel: v.Source.Label(),
		CreatedByAI: v.CreatedByAI,
		CreatedBy:   v.CreatedBy, ReviewedBy: v.ReviewedBy, PostedBy: v.PostedBy,
		TotalDebit: v.TotalDebit(), TotalCredit: v.TotalCredit(),
		Balanced: v.IsBalanced(),
	}
	if v.PostedAt != nil {
		d.PostedAt = v.PostedAt.Format(time.RFC3339)
	}
	if v.ReversesID != nil {
		if orig, err := s.db.Vouchers().Get(ctx, *v.ReversesID); err == nil {
			d.ReversesNo = orig.No
		}
	}
	if v.VoidedBy != nil {
		if rev, err := s.db.Vouchers().Get(ctx, *v.VoidedBy); err == nil {
			d.VoidedByNo = rev.No
		}
	}

	for i, e := range v.Entries {
		name := ""
		if a, ok := tree.Get(e.AccountCode); ok {
			name = a.Name
		}
		d.Lines = append(d.Lines, VoucherLine{
			LineNo: i + 1, AccountCode: e.AccountCode, AccountName: name,
			Summary: e.Summary, Debit: e.Debit, Credit: e.Credit,
			AuxDesc:   describeAux(e.Aux, names, deptNames),
			ContactID: e.Aux.ContactID, EmployeeID: e.Aux.EmployeeID,
			DeptID: e.Aux.DeptID, ProjectID: e.Aux.ProjectID,
		})
	}

	// 状态机规则只在这里算一次
	d.CanEdit = v.CanEdit() == nil
	d.CanDelete = v.CanDelete() == nil
	d.CanPost = v.CanPost() == nil
	d.CanReverse = v.CanVoid() == nil
	if !d.CanEdit {
		d.BlockedReason = blockReason(ctx, s, v)
	}
	return d, nil
}

// blockReason 解释「为什么这张凭证不能改」。
//
// 只说「状态不允许」等于没说 —— 用户需要知道是**期间结账了**
// 还是**凭证已过账**，因为这两种情况的解法完全不同。
func blockReason(ctx context.Context, s *Service, v *voucher.Voucher) string {
	switch v.Status {
	case voucher.StatusPosted:
		if cal, err := s.db.Periods().Load(ctx); err == nil {
			if p, ok := cal.Get(v.BizDate.Year, v.BizDate.Month); ok &&
				p.Status == period.StatusClosed {
				return fmt.Sprintf(
					"该凭证所属期间 %d年%02d月 已结账。已结账期间的凭证不能修改，"+
						"如需更正请先反结账，或用红字冲销。", v.BizDate.Year, v.BizDate.Month)
			}
		}
		return "凭证已过账。已记账的凭证不能直接修改，" +
			"如需更正请用红字冲销 —— 这是《会计基础工作规范》的要求。"
	case voucher.StatusVoided:
		return "凭证已被红字冲销，只能查看。"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// VoucherLineInput 是界面提交的一条分录。
//
// 金额用 **int64 分**接收，而不是字符串：
// 界面在提交前已经用 parseYuanToCents 把「元」转成了分，
// 这里再转一次只会多一个出错的地方。
type VoucherLineInput struct {
	AccountCode string
	Summary     string
	Debit       money.Money
	Credit      money.Money
	ContactID   *int64
	EmployeeID  *int64
	DeptID      *int64
	ProjectID   *int64
}

// VoucherInput 是界面提交的一张凭证。
type VoucherInput struct {
	// ID 为 0 表示新建，否则是更新已有草稿。
	ID          int64
	Word        string
	Date        string
	Remark      string
	AttachCount int
	Lines       []VoucherLineInput
	// CreatedBy 是制单人（签署在凭证上）。
	CreatedBy string

	// Source / SourceID 是凭证来源（空表示手工录入）。
	// ★ AI 采纳路径必须传 SourceAI + CreatedByAI：
	// 落库记成 manual 会让「哪些是 AI 起草的」查不出来，
	// 而审计恰恰要从 created_by_ai 这条线追溯。
	Source      voucher.Source
	SourceID    *int64
	CreatedByAI bool
	// ExpectedUpdatedAt 是乐观锁：更新已存在草稿时带读取时看到的
	// updatedAt 原文；与库里不一致时报冲突（§2.5）。新建与内部
	// 生成路径留空。
	ExpectedUpdatedAt string
}

// build 把界面输入转成领域对象，并执行完整校验。
func (s *Service) buildVoucher(ctx context.Context, in VoucherInput) (*voucher.Voucher, error) {
	word := voucher.Word(strings.TrimSpace(in.Word))
	if word == "" {
		word = voucher.WordJi
	}
	date, err := calendar.Parse(strings.TrimSpace(in.Date))
	if err != nil {
		return nil, fmt.Errorf("记账日期 %q 格式不对，应为 YYYY-MM-DD", in.Date)
	}
	maker := strings.TrimSpace(in.CreatedBy)
	if maker == "" {
		maker = defaultMaker
	}

	v, err := voucher.New(word, date, maker)
	if err != nil {
		return nil, err
	}
	v.ID = in.ID
	v.Remark = strings.TrimSpace(in.Remark)
	if in.AttachCount < 0 {
		return nil, &InvalidInput{Msg: "附单据数不能为负"}
	}
	v.AttachCount = in.AttachCount
	v.CreatedByAI = in.CreatedByAI
	if in.Source != "" {
		if err := v.SetSource(in.Source, in.SourceID); err != nil {
			return nil, err
		}
	} else if in.ID != 0 {
		// 更新路径：来源是历史事实不是入参 —— 置空让落库层
		// 沿用旧值。New 默认的 manual 不拦的话，银行流水/发票/
		// 报销生成的草稿被改一笔摘要，来源就静默刷成 manual。
		v.Source = ""
		v.SourceID = nil
	}

	for i, l := range in.Lines {
		code := strings.TrimSpace(l.AccountCode)
		summary := strings.TrimSpace(l.Summary)
		// 整行空白的直接跳过：界面上总有一行是空着等录入的
		if code == "" && summary == "" && l.Debit.IsZero() && l.Credit.IsZero() {
			continue
		}
		if err := v.AddEntry(ledger.Entry{
			AccountCode: code, Summary: summary,
			Debit: l.Debit, Credit: l.Credit,
			Aux: ledger.Aux{
				ContactID: l.ContactID, EmployeeID: l.EmployeeID,
				DeptID: l.DeptID, ProjectID: l.ProjectID,
			},
		}); err != nil {
			return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
		}
	}
	if len(v.Entries) == 0 {
		return nil, voucher.ErrNoEntries
	}
	v.Period = period.NewKey(date.Year, date.Month)
	return v, nil
}

// defaultMaker 是没填制单人时的默认署名。
//
// 不强制要求填：小微企业的老板常常既是制单人又是记账人，
// 每次弹一个「请填写制单人」只会让人烦。但凭证上必须**有**签名，
// 因此给一个明确的默认值，界面上可以随时改。
const defaultMaker = "制单人"

// CheckResult 是一次凭证校验的结论。
//
// ★ 用显式的 OK 字段，而不是让调用方从消息文本里找「不平衡」三个字：
// 文案随时会改，靠字符串匹配判断成败是个定时炸弹。
type CheckResult struct {
	OK      bool
	Message string
}

// CheckVoucher 只校验不保存，返回一句给用户看的话。
//
// 界面上的「检查一下」按钮用它：让用户在保存前就知道哪里不对，
// 而不是填了半小时才被拒绝。
func (s *Service) CheckVoucher(ctx context.Context, in VoucherInput) (CheckResult, error) {
	v, err := s.buildVoucher(ctx, in)
	if err != nil {
		// 连领域对象都构造不出来（日期格式错、分录为空）：
		// 这同样是「可以改」的问题，作为结论返回而不是错误
		return CheckResult{OK: false, Message: err.Error()}, nil
	}
	// ★ 借贷平衡要先单独判一次。
	//
	// 它是录入过程中最需要盯的一件事，而且**必须作为「结论」返回**
	// 而不是当作校验失败 —— 界面要把它显示在平衡提示条上，
	// 让人一边录一边看见差额在缩小，而不是点一下按钮弹一个错误框。
	if len(v.Entries) > 0 && !v.IsBalanced() {
		return CheckResult{OK: false, Message: fmt.Sprintf(
			"借贷不平衡：借 %s ≠ 贷 %s，差额 %s。差额不会被自动抹平 —— 请核对该记哪一边。",
			v.TotalDebit(), v.TotalCredit(),
			v.TotalDebit().Sub(v.TotalCredit()))}, nil
	}

	lctx, err := s.db.Vouchers().LedgerContext(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	per, err := v.ValidateDraft(lctx)
	if err != nil {
		return CheckResult{OK: false, Message: err.Error()}, nil
	}
	return CheckResult{OK: true, Message: fmt.Sprintf(
		"校验通过：%d 条分录，借 %s = 贷 %s，期间 %s",
		len(v.Entries), v.TotalDebit(), v.TotalCredit(), per.Key)}, nil
}

// SaveVoucher 保存一张草稿（新建或更新）。
func (s *Service) SaveVoucher(ctx context.Context, in VoucherInput) (*VoucherDetail, error) {
	// ★ 「修改前」快照必须先于 SaveDraft 读取。
	//
	// 原来在保存之后才去读原凭证 —— 此时新值已经落库，
	// 读回来的「修改前」就是「修改后」，修改留痕形同虚设。
	var before *VoucherDetail
	if in.ID != 0 {
		if b, berr := s.Voucher(ctx, in.ID); berr == nil {
			before = b
		}
	}
	v, err := s.buildVoucher(ctx, in)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Vouchers().SaveDraft(ctx, sqlite.DraftInput{
		Voucher: v, CreatedBy: v.CreatedBy,
		ExpectedUpdatedAt: in.ExpectedUpdatedAt,
	})
	if err != nil {
		return nil, err
	}
	// ★ 规范点名要记「凭证录入 / 修改」，而且修改要记**修改前后的内容**。
	// 摘要、金额、分录都在 detail 里，会计监督人员据此能还原改了什么。
	action := audit.ActionVoucherCreate
	summary := "录入凭证草稿"
	detail := map[string]any{
		"记账日期": v.BizDate.String(),
		"摘要":   v.Remark,
		"借方合计": money.Money(v.TotalDebit()).String(),
		"贷方合计": money.Money(v.TotalCredit()).String(),
		"分录":   auditLines(v),
		"制单人":  v.CreatedBy,
	}
	if in.ID != 0 {
		action = audit.ActionVoucherUpdate
		summary = "修改凭证草稿 #" + strconv.FormatInt(in.ID, 10)
		// 改之前的原样留在日志里 —— 这是「修改前后内容」的前一半
		if before != nil {
			detail["修改前"] = map[string]any{
				"记账日期": before.Date, "摘要": before.Remark,
				"借方合计": money.Money(before.TotalDebit).String(),
				"贷方合计": money.Money(before.TotalCredit).String(),
			}
		}
	}
	s.recordAudit(ctx, AuditEvent{
		Action: action, Summary: summary,
		Entity: "voucher", EntityID: strconv.FormatInt(res.VoucherID, 10),
		Operator: v.CreatedBy, Detail: detail,
	})
	return s.Voucher(ctx, res.VoucherID)
}

// auditLines 把分录压缩成日志明细。
//
// 只留「科目 + 借贷」：摘要与辅助核算的 id 已在凭证里，
// 日志记到能还原「这笔账动了哪些科目多少钱」就够 ——
// 记全文会让每条日志都很大，而日志是要长期留存的。
func auditLines(v *voucher.Voucher) []map[string]any {
	out := make([]map[string]any, 0, len(v.Entries))
	for _, e := range v.Entries {
		out = append(out, map[string]any{
			"科目": e.AccountCode, "摘要": e.Summary,
			"借方": money.Money(e.Debit).String(),
			"贷方": money.Money(e.Credit).String(),
		})
	}
	return out
}

// DeleteVoucher 删除一张草稿。
//
// ★ 规范点名要记「尚未记账凭证的删除」—— 草稿删掉就没了，
// 日志是它存在过的唯一证据，所以删除前先把内容抄进日志。
func (s *Service) DeleteVoucher(ctx context.Context, id int64, expectedUpdatedAt string) error {
	before, err := s.Voucher(ctx, id)
	if err != nil {
		return err
	}
	delRes, err := s.db.Vouchers().DeleteDraft(ctx, id, expectedUpdatedAt)
	if err != nil {
		return err
	}
	detail := map[string]any{
		"记账日期": before.Date, "摘要": before.Remark,
		"借方合计": money.Money(before.TotalDebit).String(),
		"贷方合计": money.Money(before.TotalCredit).String(),
		"制单人":  before.CreatedBy,
	}
	lines := make([]map[string]any, 0, len(before.Lines))
	for _, l := range before.Lines {
		lines = append(lines, map[string]any{
			"科目": l.AccountCode, "摘要": l.Summary,
			"借方": money.Money(l.Debit).String(),
			"贷方": money.Money(l.Credit).String(),
		})
	}
	detail["被删除的分录"] = lines
	// 删除草稿会顺带把生成它的单据退回生成前状态
	// （发票回已认证、报销回已审批、流水回已匹配……）——
	// 用户得知道删的不只是一张凭证。
	if delRes != nil {
		unlinks := map[string]int{
			"发票": delRes.UnlinkedInvoice, "报销单": delRes.UnlinkedClaim,
			"银行流水": delRes.UnlinkedFlow, "计提行": delRes.UnlinkedDepreciate,
			"摊销行": delRes.UnlinkedAmortize,
		}
		for k, n := range unlinks {
			if n > 0 {
				detail["解除关联:"+k] = n
			}
		}
	}
	// 操作人走 caller：删凭证的人和原制单人未必是同一个 —
	// 原制单人已在 detail 里（「被删除凭证的署名」是业务事实），
	// 而日志的 operator 必须回答「谁删的」。
	s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherDelete,
		Summary: "删除凭证草稿 " + firstNonEmpty(before.No, "#"+strconv.FormatInt(id, 10)),
		Entity:  "voucher", EntityID: strconv.FormatInt(id, 10),
		Detail: detail,
	})
	return nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// PostVoucher 把**一张**草稿过账。
//
// ★ 界面上没有这个动作 —— 过账只发生在账期结算。
//
// 凭证录完只落草稿：不占号、不进总账。到账期结算时，
// Service.Close 会调用 PostPeriodDrafts 把本期草稿统一过账，
// 然后才结转损益、关期间（见 internal/store/sqlite/closing_repo.go）。
//
// 这个函数保留下来，是因为「把一张草稿过账」是结算的实现细节，
// 也是测试与命令行需要的原语；它不是一条给用户走的旁路。
//
// postedBy 是记账人 —— 与制单人分开：中国实务要求制单、审核、
// 记账三个签章可追溯，小微企业往往一人身兼三职，但**字段要分开**，
// 规模变大后不需要改数据结构。
func (s *Service) PostVoucher(ctx context.Context, id int64, postedBy string) (*VoucherDetail, error) {
	if strings.TrimSpace(postedBy) == "" {
		return nil, errors.New("请填写记账人 —— 记账凭证需要有记账签章")
	}
	v, err := s.db.Vouchers().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := v.CanPost(); err != nil {
		return nil, err
	}
	accounts, err := s.db.Accounts().IDsByCode(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Vouchers().Post(ctx, sqlite.PostInput{
		Voucher: v, Accounts: accounts,
		PostingBy: strings.TrimSpace(postedBy), At: time.Now(),
	}); err != nil {
		return nil, err
	}
	s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherPost,
		Summary: "凭证过账 " + firstNonEmpty(v.No, "#"+strconv.FormatInt(id, 10)),
		Entity:  "voucher", EntityID: strconv.FormatInt(id, 10),
		Operator: strings.TrimSpace(postedBy),
		Detail: map[string]any{
			"凭证号": v.No, "记账日期": v.BizDate.String(), "摘要": v.Remark,
			"借方合计": money.Money(v.TotalDebit()).String(),
			"贷方合计": money.Money(v.TotalCredit()).String(),
			"制单人":  v.CreatedBy, "记账人": strings.TrimSpace(postedBy),
			"分录": auditLines(v),
		},
	})
	return s.Voucher(ctx, id)
}

// PostPeriodResult 是一次「过账本期全部草稿」的结果。
type PostPeriodResult struct {
	Period string `json:"period"`
	// Posted 是过账的张数。
	Posted int `json:"posted"`
	// Nos 是分配到的凭证号。
	Nos []string `json:"nos"`
}

// PostPeriodDrafts 把某一期间的全部草稿凭证过账。
//
// ★ 结账（Close）内部就用它，界面上没有单独的按钮。
//
// 之所以还导出：命令行与测试需要「把这一期记进账」而不结账
// （结账还会结转损益、关期间）。**不要**拿它做一条给用户走的旁路 ——
// 用户能随时点「过账」的话，「凭证号连续」「期间完整」就又变回
// 靠人自觉的事了。
func (s *Service) PostPeriodDrafts(ctx context.Context, k period.Key,
	postedBy string) (*PostPeriodResult, error) {
	if strings.TrimSpace(postedBy) == "" {
		return nil, errors.New("请填写记账人 —— 记账凭证需要有记账签章")
	}
	if !k.Valid() {
		return nil, fmt.Errorf("会计期间 %04d-%02d 非法", k.Year, k.Month)
	}
	res, err := s.db.Vouchers().PostPeriodDrafts(ctx, k, strings.TrimSpace(postedBy), time.Now())
	if err != nil {
		return nil, err
	}
	if res.Posted > 0 {
		s.recordAudit(ctx, AuditEvent{
			Action: audit.ActionVoucherPost, Summary: fmt.Sprintf("过账 %s 全部草稿", k),
			Entity: "period", EntityID: k.String(), Operator: strings.TrimSpace(postedBy),
			Detail: map[string]any{
				"期间": k.String(), "张数": res.Posted, "凭证号": res.Nos,
			},
		})
	}
	return &PostPeriodResult{
		Period: k.String(), Posted: res.Posted, Nos: nonNilSlice(res.Nos),
	}, nil
}

// ReverseVoucher 红字冲销一张已过账凭证。
func (s *Service) ReverseVoucher(ctx context.Context, id int64, by, date string) (*VoucherDetail, error) {
	if strings.TrimSpace(by) == "" {
		return nil, errors.New("请填写操作人")
	}
	var bizDate calendar.Date
	if strings.TrimSpace(date) != "" {
		d, err := calendar.Parse(strings.TrimSpace(date))
		if err != nil {
			return nil, fmt.Errorf("冲销日期 %q 格式不对，应为 YYYY-MM-DD", date)
		}
		bizDate = d
	}
	orig, err := s.Voucher(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Vouchers().Reverse(ctx, sqlite.ReverseInput{
		VoucherID: id, By: strings.TrimSpace(by), BizDate: bizDate, At: time.Now(),
	})
	if err != nil {
		return nil, err
	}
	det, err := s.Voucher(ctx, res.VoucherID)
	if err != nil {
		return nil, err
	}
	// ★ 冲销是改账动作，成功也必须进操作日志：
	// 凭证上虽然留着 voided_by 的追溯关系，但「谁在什么时候
	// 冲销了哪张凭证」是审计要单独回答的问题。
	s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherReverse,
		Summary: fmt.Sprintf("冲销凭证 %s", orig.No),
		Entity:  "voucher", EntityID: fmt.Sprintf("%d", id),
		Operator: strings.TrimSpace(by),
		Detail: map[string]any{
			"原凭证号":   orig.No,
			"原凭证ID":  id,
			"冲销凭证号":  det.No,
			"冲销凭证ID": res.VoucherID,
			"冲销日期":   det.Date,
		},
	})
	return det, nil
}

// ---------------------------------------------------------------------------
// 录入辅助
// ---------------------------------------------------------------------------

// AccountOption 是科目选择器的一项。
type AccountOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// FullName 含上级名称，如「管理费用—办公费」。
	FullName string `json:"fullName"`
	// Direction 是余额方向：借 / 贷。
	Direction string `json:"direction"`
	// AuxTypes 是需要的辅助核算维度（中文名）。
	AuxTypes []string `json:"auxTypes"`
	// AuxKeys 是对应的原始维度 key（customer|supplier|employee|dept|project）。
	//
	// ★ 界面选档案要靠它而不是中文名 —— 「应收」科目要的是 customer，
	// 拿中文「客户」去反推 key 迟早断。原来没发它，于是辅助核算
	// 弹层只会列往来单位，员工/部门/项目永远选不到。
	AuxKeys []string `json:"auxKeys"`
	// SearchText 是给前端做模糊匹配用的预拼接串。
	//
	// ★ 在 Go 侧拼好而不是让前端每次输入都遍历 190 个科目拼一遍：
	// 拼串只在科目表加载时做一次，输入时只是 indexOf。
	SearchText string `json:"searchText"`
}

// AccountOptions 返回可记账科目，供录入界面选择。
func (s *Service) AccountOptions(ctx context.Context) ([]AccountOption, error) {
	tree, err := s.db.Accounts().Tree(ctx)
	if err != nil {
		return nil, err
	}
	all := tree.All()
	byCode := make(map[string]*account.Account, len(all))
	for _, a := range all {
		byCode[a.Code] = a
	}

	var out []AccountOption
	for _, a := range tree.EnabledLeaves() {
		aux := make([]string, 0, len(a.AuxTypes))
		keys := make([]string, 0, len(a.AuxTypes))
		for _, t := range a.AuxTypes {
			aux = append(aux, t.Label())
			keys = append(keys, string(t))
		}
		dir := "借"
		if a.BalanceDir == account.DirCredit {
			dir = "贷"
		}
		full := a.FullName(byCode)
		out = append(out, AccountOption{
			Code: a.Code, Name: a.Name, FullName: full,
			Direction: dir, AuxTypes: aux, AuxKeys: keys,
			SearchText: strings.ToLower(a.Code + " " + full + " " + a.Name),
		})
	}
	return out, nil
}

// ContactOption 是往来单位选择器的一项。
type ContactOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// KindLabel 是类型中文名。
	KindLabel string `json:"kindLabel"`
	// ShortName 是简称，银行流水里出现的常是它。
	ShortName string `json:"shortName"`
}

// ContactOptions 返回全部启用中的往来单位。
func (s *Service) ContactOptions(ctx context.Context) ([]ContactOption, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, name, kind, short_name FROM contact
		 WHERE is_enabled = 1 ORDER BY kind, name`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []ContactOption
	for rows.Next() {
		var c ContactOption
		var kind string
		if err := rows.Scan(&c.ID, &c.Name, &kind, &c.ShortName); err != nil {
			return nil, err
		}
		c.Kind = kind
		c.KindLabel = contactKindName(kind)
		out = append(out, c)
	}
	return out, rows.Err()
}

func contactKindName(kind string) string {
	switch kind {
	case "customer":
		return "客户"
	case "supplier":
		return "供应商"
	case "employee":
		return "员工"
	case "shareholder":
		return "股东"
	case "both":
		return "客户/供应商"
	default:
		return "其他单位"
	}
}

// contactNames 返回 id → 名称的映射，用于渲染辅助核算描述。
func (s *Service) contactNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id, name FROM contact`)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// describeAux 把辅助核算渲染成中文描述。
//
// 用**名称**而不是 `往来#3` 这样的 id：凭证详情是给人看的，
// 「张三」「管理部门」比数字有用得多。
//
// ★ 部门名原来是没查的，凭证上一直显示「部门#1」——
// 用户拿着一沓凭证根本对不出那是哪个部门。部门档案表（0008）补上之后
// 就能查名字了，这里跟上。员工同理，等档案齐了再补。
func describeAux(a ledger.Aux, names map[int64]string, deptNames map[int64]string) string {
	var parts []string
	if a.ContactID != nil {
		if n, ok := names[*a.ContactID]; ok {
			parts = append(parts, n)
		} else {
			parts = append(parts, fmt.Sprintf("往来#%d", *a.ContactID))
		}
	}
	if a.EmployeeID != nil {
		parts = append(parts, fmt.Sprintf("员工#%d", *a.EmployeeID))
	}
	if a.DeptID != nil {
		if n, ok := deptNames[*a.DeptID]; ok && n != "" {
			parts = append(parts, n)
		} else {
			parts = append(parts, fmt.Sprintf("部门#%d", *a.DeptID))
		}
	}
	if a.ProjectID != nil {
		parts = append(parts, fmt.Sprintf("项目#%d", *a.ProjectID))
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// 附件
// ---------------------------------------------------------------------------

// AttachToVoucher 把附件挂到凭证上。
//
// # 为什么必须写 attachment 表
//
// 附件实体按内容寻址存在 <账套>.files/ 下，而**归属关系**必须入库。
// 只把文件写到磁盘、只把 attach_count 加一，会有三个后果：
//
//  1. 打开凭证时列不出它有哪些附件 —— 文件在，但没人知道是谁的
//  2. 孤儿附件清理无从判断「这个文件还有没有人引用」，只能全留或全删
//  3. 备份虽然会带上文件（按目录全量打包），但恢复后依然不知道该挂给谁
//
// 因此这里一次事务内做三件事：校验凭证、写 attachment、返回附件信息。
// 任何一步失败都整体回滚 —— 不会出现「文件在但没记录」或
// 「记录在但文件丢了」的半截状态。
//
// ★ 不回写 voucher.attach_count：那一列是用户手填的「附单据数」
// （原始纸质单据的张数，要跟实物核对），不是电子附件数。
// 用电子附件数覆盖它，用户填的「附 3 张发票」会在挂上一个 PDF 后
// 变成 1 —— 静默丢数据。电子附件数以 attachment 表实计为准
// （AttachmentAudit 按 EXISTS 统计）。
func (s *Service) AttachToVoucher(ctx context.Context, voucherID int64,
	filename string, data []byte) (*AttachmentInfo, error) {
	return s.AttachDocument(ctx, "voucher", voucherID, filename, data)
}

// attachmentOwners 是 attachment.owner_type 的合法归属
// （与表 CHECK 一致）：表名用于「确认归属存在」，label 用于
// 审计摘要（与既有中文口径一致）。表名全部字面量写死 ——
// 用户输入只能选键，绝不进 SQL。
var attachmentOwners = map[string]struct {
	table, label string
}{
	"voucher":       {"voucher", "凭证"},
	"invoice":       {"invoice", "发票"},
	"expense_claim": {"expense_claim", "报销单"},
	"expense_item":  {"expense_item", "报销明细"},
}

// AttachDocument 把一份附件挂到某类单据上（凭证/发票/报销单/报销明细）。
// owner 必须先存在 —— 往不存在的 id 上挂会留下「文件在、归属悬空」
// 的孤儿记录；类型不在白名单内直接拒绝。
func (s *Service) AttachDocument(ctx context.Context, ownerType string,
	ownerID int64, filename string, data []byte) (*AttachmentInfo, error) {

	own, ok := attachmentOwners[ownerType]
	if !ok {
		return nil, &InvalidInput{
			Msg: fmt.Sprintf("附件归属类型 %q 非法（只接受 voucher|invoice|expense_claim|expense_item）", ownerType)}
	}

	// ★ 先查归属再落盘：顺序反过来时，往不存在的 id 上传会
	// 把文件写进 .files/ 再报「归属不存在」—— 调用方拿到错误，
	// 磁盘上却多了一个能读、能进孤儿清单、能进备份的实物文件。
	var n int
	if err := s.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+own.table+` WHERE id = ?`, ownerID).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: %s id=%d", sqlite.ErrNotFound, ownerType, ownerID)
	}

	store, err := s.Attachments()
	if err != nil {
		return nil, err
	}
	// ★ 入库失败要能把「这次新写的文件」收回去：记下落盘前
	// 主目录里这个内容在不在 —— 已存在的（别的单据共享着的）
	// 文件绝不能删。判定只看主目录路径：回退目录里有同内容
	// 文件不代表主目录这份是共享的。
	sum := sha256.Sum256(data)
	preHash := hex.EncodeToString(sum[:])
	_, statErr := os.Stat(store.MustPath(preHash))
	existed := statErr == nil
	blob, err := store.Put(data, filename)
	if err != nil {
		return nil, err
	}

	err = s.db.WithTx(ctx, func(tx *sqliteTx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		// 事务内复查：上面那次检查到落库之间归属可能被删掉，
		// 仍按「不存在」处理 —— 此时文件已落盘，会留孤儿文件，
		// 但那属于删除方的并发代价，孤儿清理可以收拾它。
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM `+own.table+` WHERE id = ?`, ownerID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s id=%d", sqlite.ErrNotFound, ownerType, ownerID)
		}
		// 同一单据挂同一份文件不重复：UNIQUE(owner_type, owner_id, sha256)
		// 兜底，这里用 INSERT OR IGNORE 让重复上传变成幂等操作 ——
		// 用户重复拖同一个 PDF 进来是很常见的动作，报错没有意义。
		if _, err := tx.Exec(ctx, `
			INSERT OR IGNORE INTO attachment
				(owner_type, owner_id, file_name, sha256, mime, size, kind, created_at)
			VALUES (?, ?, ?, ?, ?, ?, '', ?)`,
			ownerType, ownerID, blob.FileName, blob.SHA256, blob.Mime, blob.Size, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		// ★ 入库失败要收回这次新落的文件：不然调用方拿到错误，
		// 磁盘上却多出一份能读、能进孤儿清单、能进备份的实物。
		// 只删「本来不在 + 此刻仍无人引用」的 —— 并发传同一份
		// 文件的另一个上传可能已经把关联落库，删了它就让那笔
		// 关联指向缺失文件。
		if !existed {
			var refs int
			_ = s.db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM attachment WHERE sha256 = ?`,
				blob.SHA256).Scan(&refs)
			var evRefs int
			var hasEvidence int
			if s.db.SQL().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='audit_evidence'`).
				Scan(&hasEvidence) == nil && hasEvidence > 0 {
				_ = s.db.SQL().QueryRowContext(ctx,
					`SELECT COUNT(*) FROM audit_evidence WHERE sha256 = ?`,
					blob.SHA256).Scan(&evRefs)
			}
			if refs == 0 && evRefs == 0 {
				if derr := store.Delete(blob.SHA256); derr != nil {
					// 删不掉不盖过原始错误 —— 孤儿清理还能收拾。
					_, _ = os.Stderr.WriteString(
						"[attachment] 失败上传的残留文件清理失败: " + derr.Error() + "\n")
				}
			}
		}
		return nil, err
	}
	// ★ 挂附件是单据内容的变更，必须留痕：
	// 审计查「这张凭证的发票是什么时候补上的」就靠这一条。
	s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionAttachmentUpload,
		Summary: fmt.Sprintf("给%s #%d 挂附件 %s", own.label, ownerID, blob.FileName),
		Entity:  ownerType, EntityID: strconv.FormatInt(ownerID, 10),
		Source: audit.SourceGUI,
		Detail: map[string]any{
			"文件名": blob.FileName, "SHA256": blob.SHA256,
			"大小": blob.Size, "归属": ownerType, "归属ID": ownerID,
		},
	})
	return &AttachmentInfo{
		Hash: blob.SHA256, Name: blob.FileName, Size: blob.Size,
		MIME: blob.Mime, Path: store.MustPath(blob.SHA256),
	}, nil
}

// ListAttachments 返回某单据的附件列表。
func (s *Service) ListAttachments(ctx context.Context, ownerType string, ownerID int64) ([]AttachmentInfo, error) {
	store, err := s.Attachments()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT file_name, sha256, mime, size FROM attachment
		 WHERE owner_type = ? AND owner_id = ?
		 ORDER BY id`, ownerType, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []AttachmentInfo
	for rows.Next() {
		var a AttachmentInfo
		if err := rows.Scan(&a.Name, &a.Hash, &a.MIME, &a.Size); err != nil {
			return nil, err
		}
		// hash 从库里读出来，但也走校验版：库里若被外部工具改坏，
		// 这里退化成一个「指向不存在位置」的路径，而不是越界路径。
		// ResolvePath 取实际落点 —— 文件可能还在旧布局回退目录。
		a.Path, _ = store.ResolvePath(a.Hash)
		// 文件实体可能被误删（用户手动清理 .files 目录）。
		// 这里如实标出来，界面才能提示「原件已丢失」而不是
		// 让用户点开一个不存在的路径。
		a.Missing = !store.Exists(a.Hash)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAttachment 解除某单据与附件的关系。
//
// 只删数据库记录，**不删磁盘上的文件实体**：内容寻址下同一份文件
// 可能被多个单据共享（同一张发票挂在发票档案与报销单两边），
// 删实体要扫描全部引用，代价高且容易误删。
// 真正的清理由「孤儿附件清理」统一做（见 OrphanAttachments）。
func (s *Service) DeleteAttachment(ctx context.Context, ownerType string, ownerID int64, hash string) error {
	var fileName string
	var deleted int64
	err := s.db.WithTx(ctx, func(tx *sqliteTx) error {
		// 文件名先取出来给审计明细用 —— 删掉之后就查不到了。
		_ = tx.QueryRow(ctx, `
			SELECT file_name FROM attachment
			 WHERE owner_type = ? AND owner_id = ? AND sha256 = ?`,
			ownerType, ownerID, hash).Scan(&fileName)
		// ★ 同 AttachToVoucher：不回写 voucher.attach_count ——
		// 那是纸质附单据数，删一个电子附件不该改掉它。
		res, err := tx.Exec(ctx, `
			DELETE FROM attachment
			 WHERE owner_type = ? AND owner_id = ? AND sha256 = ?`,
			ownerType, ownerID, hash)
		if err != nil {
			return err
		}
		deleted, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	// 解除关联与上传一样要留痕 —— 「这张凭证的原件是谁摘掉的」
	// 是审计必答的问题；没删到行（重复删/张冠李戴）不记。
	if deleted > 0 {
		label := ownerType
		if own, ok := attachmentOwners[ownerType]; ok {
			label = own.label
		}
		s.recordAudit(ctx, AuditEvent{
			Action:  audit.ActionAttachmentDelete,
			Summary: fmt.Sprintf("摘掉%s #%d 的附件 %s", label, ownerID, fileName),
			Entity:  ownerType, EntityID: strconv.FormatInt(ownerID, 10),
			Source: audit.SourceGUI,
			Detail: map[string]any{
				"文件名": fileName, "SHA256": hash,
				"归属": ownerType, "归属ID": ownerID,
			},
		})
	}
	return nil
}

// OrphanAttachments 返回磁盘上存在、但没有任何单据引用的附件。
//
// 用途是「附件清理」：用户删了发票、换了凭证，文件会留在 .files 里。
// 只**报告**不自动删除 —— 自动删用户数据的代价太大，
// 而这个操作本来就不常做，让人看一眼再决定更稳妥。
func (s *Service) OrphanAttachments(ctx context.Context) ([]AttachmentInfo, error) {
	store, err := s.Attachments()
	if err != nil {
		return nil, err
	}
	hashes, err := store.List()
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	// ★ 可达的 sha256 有两处：attachment 表（凭证/发票/报销单的附件）
	// 与 audit_evidence 表（审计证据链上挂的原件）。
	//
	// 只查 attachment 会怎样：审计底稿上那份「折旧计算表」被列出成
	// 孤儿文件，而界面在旁边写着「请自行确认后再清理 .files 目录」——
	// 用户照着做，底稿的全部依据就变成「原件已丢失」。
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT DISTINCT sha256 FROM attachment WHERE sha256 <> ''
		UNION
		SELECT DISTINCT sha256 FROM audit_evidence WHERE sha256 <> ''`)
	if err != nil {
		return nil, translate(err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, err
		}
		used[h] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []AttachmentInfo
	for _, h := range hashes {
		if used[h] {
			continue
		}
		var size int64
		if b, err := store.Get(h); err == nil {
			size = int64(len(b))
		}
		out = append(out, AttachmentInfo{Hash: h, Size: size, Path: store.MustPath(h)})
	}
	return out, nil
}

// AttachmentAudit 报告某期间已记账凭证的电子附件覆盖情况。
//
// 这是「对账」的一部分：《会计基础工作规范》要求记账凭证附有原始单据，
// 而小微企业的现实是「发票在微信里、报销单在抽屉里」。
// 这个数字让会计知道还差多少张没补。
type AttachmentAudit struct {
	Posted     int `json:"posted"`
	WithAttach int `json:"withAttach"`
	Without    int `json:"without"`
}

// AttachmentCoverage 统计凭证的附件覆盖情况。
//
// ★ 「有附件」按 attachment 表 EXISTS 判定，不看 voucher.attach_count ——
// 那一列是用户手填的纸质「附单据数」，语义不同：填了附单据数但
// 没扫描上传的凭证，恰恰是这项统计要抓出来的对象。
//
// year/month 任一 <=0 表示统计全部期间；否则先经 period.NewKey 校验，
// 非法年月直接拒绝（如 2025-13 会静默查出一堆 0，看起来像「都齐了」）。
func (s *Service) AttachmentCoverage(ctx context.Context,
	year, month int) (*AttachmentAudit, error) {

	var where string
	var args []any
	if year > 0 || month > 0 {
		k := period.NewKey(year, month)
		if !k.Valid() {
			return nil, fmt.Errorf("会计期间 %04d-%02d 非法", year, month)
		}
		where = " AND year = ? AND month = ?"
		args = []any{year, month}
	}
	// ★ 「有附件」必须算上文件实体：只查关联行时，原件被删
	// 的凭证仍被计进 withAttach —— 覆盖率虚高，审计拿它当
	// 「原始单据齐全」的证据就错了。逐 hash 校验 store.Exists，
	// 有任一附件实体在才算有附件。
	store, err := s.Attachments()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT v.id, a.sha256
		  FROM voucher v
		  LEFT JOIN attachment a
		    ON a.owner_type = 'voucher' AND a.owner_id = v.id
		 WHERE v.status = 'posted'`+where, args...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	audit := &AttachmentAudit{}
	seen := map[int64]bool{}    // 已计进 Posted 的凭证
	hasFile := map[int64]bool{} // 已计进 WithAttach 的凭证
	present := map[string]bool{}
	for rows.Next() {
		var vid int64
		var hash *string
		if err := rows.Scan(&vid, &hash); err != nil {
			return nil, translate(err)
		}
		if !seen[vid] {
			seen[vid] = true
			audit.Posted++
		}
		if hash == nil || hasFile[vid] {
			continue
		}
		exists, ok := present[*hash]
		if !ok {
			exists = store.Exists(*hash)
			present[*hash] = exists
		}
		if exists {
			hasFile[vid] = true
			audit.WithAttach++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	audit.Without = audit.Posted - audit.WithAttach
	return audit, nil
}

// AttachmentInfo 是一个已保存的附件。
type AttachmentInfo struct {
	Hash string `json:"hash"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	MIME string `json:"mime"`
	// Path 是附件在磁盘上的绝对路径。
	Path string `json:"path"`
	// Missing 为真表示数据库里有记录、但磁盘上的文件不见了。
	//
	// 用户手动清理 .files 目录、或从别处拷来一个不完整的账套时会出现。
	// 如实标出来，界面才能提示「原件已丢失」而不是让用户
	// 点开一个不存在的路径。
	Missing bool `json:"missing"`
}
