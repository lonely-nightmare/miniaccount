package mcpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/attachment"
	"miniaccount/internal/service"
)

// 读工具的输入类型 —— jsonschema 标签即工具说明里的字段描述。

type bookInfoIn struct{}

type listAccountsIn struct{}

type listPeriodsIn struct {
	pageIn
}

type queryVouchersIn struct {
	Year          int    `json:"year,omitempty" jsonschema:"会计年度，0 表示不限"`
	Month         int    `json:"month,omitempty" jsonschema:"会计月份，0 表示不限"`
	Status        string `json:"status,omitempty" jsonschema:"draft|posted|voided，空表示不限"`
	Keyword       string `json:"keyword,omitempty" jsonschema:"按摘要或凭证号模糊匹配"`
	AccountPrefix string `json:"accountPrefix,omitempty" jsonschema:"只留分录里出现过该前缀科目的凭证"`
	Limit         int    `json:"limit,omitempty" jsonschema:"每页行数，默认 100，上限 500"`
	Offset        int    `json:"offset,omitempty" jsonschema:"跳过的行数，分页用"`
}

type getVoucherIn struct {
	ID int64 `json:"id" jsonschema:"凭证 id"`
}

type getLedgerIn struct {
	AccountPrefix string `json:"accountPrefix" jsonschema:"科目编码前缀，如 1002、2241"`
	From          string `json:"from,omitempty" jsonschema:"起始日期 YYYY-MM-DD，留空为账套全期间"`
	To            string `json:"to,omitempty" jsonschema:"截止日期 YYYY-MM-DD，留空为账套全期间"`
	Limit         int    `json:"limit,omitempty" jsonschema:"行数上限，默认 2000"`
}

type periodIn struct {
	Year  int `json:"year" jsonschema:"会计年度"`
	Month int `json:"month" jsonschema:"会计月份 1-12"`
}

type balanceSheetIn struct {
	AsOf string `json:"asOf" jsonschema:"报表日期 YYYY-MM-DD"`
}

type listContactsIn struct {
	Kind string `json:"kind,omitempty" jsonschema:"customer|supplier|both|shareholder|other，空表示全部"`
	pageIn
}

type listDepartmentsIn struct {
	pageIn
}

type listEmployeesIn struct {
	OnlyEnabled bool `json:"onlyEnabled,omitempty" jsonschema:"为真只返回在职启用员工"`
	pageIn
}

type listInvoicesIn struct {
	Direction string `json:"direction,omitempty" jsonschema:"input|output（可缩写 in|out），空表示全部"`
	Status    string `json:"status,omitempty" jsonschema:"pending|verified|booked|voided，空表示全部"`
	From      string `json:"from,omitempty" jsonschema:"开票日期起 YYYY-MM-DD"`
	To        string `json:"to,omitempty" jsonschema:"开票日期止 YYYY-MM-DD"`
	pageIn
}

type listBankFlowsIn struct {
	Status string `json:"status,omitempty" jsonschema:"imported|matched|posted|ignored，空表示全部"`
	From   string `json:"from,omitempty" jsonschema:"日期起 YYYY-MM-DD"`
	To     string `json:"to,omitempty" jsonschema:"日期止 YYYY-MM-DD"`
	pageIn
}

// registerTools 注册 14 读 + 6 写。
func (s *Server) registerTools() {
	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "book_info",
		Description: "账套信息：名称、公司、纳税人身份、会计期间列表",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ bookInfoIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			b, err := svc.Book(ctx)
			if err != nil {
				return nil, err
			}
			return mustJSON(b), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_accounts",
		Description: "科目表：编码、名称、类别、辅助核算要求",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ listAccountsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Accounts(ctx)
			if err != nil {
				return nil, err
			}
			return mustJSON(v), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_periods",
		Description: "会计期间列表及各期间状态（未启用/启用/已结账）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listPeriodsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			b, err := svc.Book(ctx)
			if err != nil {
				return nil, err
			}
			return pageOf(b.Periods, in.pageIn), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "query_vouchers",
		Description: "分页查凭证（不含分录）；可按期间/状态/关键字/科目前缀过滤",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in queryVouchersIn) (*mcp.CallToolResult, any, error) {
		if err := enumIn(in.Status, voucherStatuses, "status"); err != nil {
			return toolError(err)
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			p, err := svc.VouchersPage(ctx, service.VoucherQuery{
				Year: in.Year, Month: in.Month, Status: in.Status,
				Keyword: in.Keyword, AccountPrefix: in.AccountPrefix,
				Limit: in.Limit, Offset: in.Offset,
			})
			if err != nil {
				return nil, err
			}
			return yuanize(p), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "get_voucher",
		Description: "一张凭证的完整内容：分录、辅助核算、乐观锁戳 updatedAt",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getVoucherIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			d, err := svc.Voucher(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			return mustJSON(voucherOut(d)), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "get_ledger",
		Description: "某科目的明细账（仅已过账分录），含逐行累计余额",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getLedgerIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			r, err := svc.LedgerDetail(ctx, service.LedgerDetailQuery{
				AccountPrefix: in.AccountPrefix, From: in.From, To: in.To, Limit: in.Limit,
			})
			if err != nil {
				return nil, err
			}
			return yuanize(r), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "trial_balance",
		Description: "某期间的试算平衡表（仅已过账分录）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in periodIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			r, err := svc.TrialBalance(ctx, in.Year, in.Month)
			if err != nil {
				return nil, err
			}
			s.recordReportView(ctx, svc, "试算平衡表",
				fmt.Sprintf("%04d-%02d", in.Year, in.Month))
			return yuanize(r), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "balance_sheet",
		Description: "截至某日的资产负债表，附勾稽检查问题清单",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in balanceSheetIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			closing, opening, issues, err := svc.BalanceSheet(ctx, in.AsOf)
			if err != nil {
				return nil, err
			}
			s.recordReportView(ctx, svc, "资产负债表", in.AsOf)
			// §2.10：勾稽异常不报错，balanced+issues[] 表达。
			return yuanize(map[string]any{
				"closing": closing, "opening": opening,
				"balanced": len(issues) == 0, "issues": issues,
			}), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "income_statement",
		Description: "某期间的利润表（本期+本年累计），附勾稽问题清单",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in periodIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			cur, ytd, issues, err := svc.IncomeStatement(ctx, in.Year, in.Month)
			if err != nil {
				return nil, err
			}
			s.recordReportView(ctx, svc, "利润表",
				fmt.Sprintf("%04d-%02d", in.Year, in.Month))
			return yuanize(map[string]any{
				"current": cur, "ytd": ytd,
				"balanced": len(issues) == 0, "issues": issues,
			}), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_contacts",
		Description: "往来单位档案（客户/供应商/股东等）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listContactsIn) (*mcp.CallToolResult, any, error) {
		if err := enumIn(in.Kind, contactKinds, "kind"); err != nil {
			return toolError(err)
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Contacts(ctx, in.Kind)
			if err != nil {
				return nil, err
			}
			return pageOf(v, in.pageIn), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_departments",
		Description: "部门列表",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listDepartmentsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Departments(ctx)
			if err != nil {
				return nil, err
			}
			return pageOf(v, in.pageIn), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_employees",
		Description: "员工列表（含工资科目、社保方案等薪酬口径字段）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listEmployeesIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Employees(ctx, in.OnlyEnabled)
			if err != nil {
				return nil, err
			}
			return pageOf(yuanize(v), in.pageIn), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_invoices",
		Description: "发票列表（进项/销项，按日期区间与状态过滤）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listInvoicesIn) (*mcp.CallToolResult, any, error) {
		// 底层域值是 input|output；文档缩写 in|out 也收，
		// 其余值显式报错——静默不匹配会返回空表，比报错难查。
		dir := in.Direction
		switch dir {
		case "in":
			dir = "input"
		case "out":
			dir = "output"
		}
		if err := enumIn(dir, invoiceDirections, "direction"); err != nil {
			return toolError(err)
		}
		if err := enumIn(in.Status, invoiceStatuses, "status"); err != nil {
			return toolError(err)
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Invoices(ctx, service.InvoiceQuery{
				Direction: dir, Status: in.Status, From: in.From, To: in.To,
			})
			if err != nil {
				return nil, err
			}
			return pageOf(yuanize(v), in.pageIn), nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_bank_flows",
		Description: "银行流水列表（导入的逐笔流水及匹配状态）",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listBankFlowsIn) (*mcp.CallToolResult, any, error) {
		// 域值是 imported|matched|posted|ignored；unmatched/booked
		// 是口误惯用值，做别名；其余非法值显式报错。
		status := in.Status
		switch status {
		case "unmatched":
			status = "imported"
		case "booked":
			status = "posted"
		}
		if err := enumIn(status, bankFlowStatuses, "status"); err != nil {
			return toolError(err)
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.BankFlows(ctx, service.BankFlowQuery{
				Status: status, From: in.From, To: in.To,
			})
			if err != nil {
				return nil, err
			}
			return pageOf(yuanize(v), in.pageIn), nil
		})
	})

	// -------------------------------------------------------------------
	// 附件（读侧）：列表/内容/孤儿/覆盖率。写侧在 writes.go。
	// 宿主机路径一律不外发 —— 远程客户端用不了，本机客户端也不该
	// 拿到「服务端磁盘布局」这种信息。
	// -------------------------------------------------------------------

	mcp.AddTool(s.core, &mcp.Tool{
		Name: "list_attachments",
		Description: "某单据的附件列表（文件名、大小、MIME、是否原件缺失）；" +
			"ownerType 为 voucher|invoice|expense_claim|expense_item",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in attachmentIn) (*mcp.CallToolResult, any, error) {
		if err := validateAttachmentOwner(in.OwnerType, in.OwnerID); err != nil {
			return toolError(err)
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			list, err := svc.ListAttachments(ctx, in.OwnerType, in.OwnerID)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(list))
			for _, a := range list {
				out = append(out, attachmentOut(a))
			}
			return map[string]any{"items": out, "total": len(out)}, nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name: "read_attachment",
		Description: "按 sha256 读取附件内容（base64）。hash 来自 " +
			"list_attachments / upload_attachment 返回。单文件上限 20MB。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in readAttachmentIn) (*mcp.CallToolResult, any, error) {
		h := strings.TrimSpace(in.Hash)
		if err := attachment.ValidateHash(h); err != nil {
			return toolError(&service.InvalidInput{Msg: "hash 必须是 64 位十六进制 sha256"})
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			store, err := svc.Attachments()
			if err != nil {
				return nil, err
			}
			data, err := store.Get(h)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"hash":       h,
				"size":       len(data),
				"mime":       http.DetectContentType(data),
				"dataBase64": base64.StdEncoding.EncodeToString(data),
			}, nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "list_orphan_attachments",
		Description: "磁盘上存在、但没有任何单据引用的附件（孤儿文件）；只报告不删除",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			list, err := svc.OrphanAttachments(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(list))
			for _, a := range list {
				out = append(out, attachmentOut(a))
			}
			return map[string]any{"items": out, "total": len(out)}, nil
		})
	})

	mcp.AddTool(s.core, &mcp.Tool{
		Name: "attachment_coverage",
		Description: "已记账凭证的电子附件覆盖率：多少张过账凭证挂了/没挂原件；" +
			"year+month 同时给按期间统计，都不给统计全部",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in attachmentCoverageIn) (*mcp.CallToolResult, any, error) {
		if (in.Year > 0) != (in.Month > 0) {
			return toolError(&service.InvalidInput{Msg: "year 与 month 必须同时给出或同时省略"})
		}
		return s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			return svc.AttachmentCoverage(ctx, in.Year, in.Month)
		})
	})

	if !s.readonly {
		s.registerWriteTools()
	}
}

// ---------------------------------------------------------------------------
// 过滤枚举
// ---------------------------------------------------------------------------
//
// 过滤参数非法时静默返回空表，比报错更难查（direction 曾因此
// 被实测抓到：文档写 in|out、底层用 input|output，按文档传值
// 永远空表）。所有枚举入参统一过 enumIn。

var voucherStatuses = map[string]struct{}{
	"draft": {}, "posted": {}, "voided": {},
}

var contactKinds = map[string]struct{}{
	"customer": {}, "supplier": {}, "both": {},
	"shareholder": {}, "other": {},
}

var invoiceDirections = map[string]struct{}{"input": {}, "output": {}}

var invoiceStatuses = map[string]struct{}{
	"pending": {}, "verified": {}, "booked": {}, "voided": {},
}

var bankFlowStatuses = map[string]struct{}{
	"imported": {}, "matched": {}, "posted": {}, "ignored": {},
}

// attachmentOwnerTypes 与 attachment 表 CHECK 一致的四类归属。
var attachmentOwnerTypes = map[string]struct{}{
	"voucher": {}, "invoice": {}, "expense_claim": {}, "expense_item": {},
}

// attachmentIn 是附件归属参数。
type attachmentIn struct {
	OwnerType string `json:"ownerType" jsonschema:"归属类型：voucher|invoice|expense_claim|expense_item"`
	OwnerID   int64  `json:"ownerId" jsonschema:"归属单据 id"`
}

type readAttachmentIn struct {
	Hash string `json:"hash" jsonschema:"附件 sha256（64 位十六进制），来自 list_attachments 或 upload_attachment"`
}

type attachmentCoverageIn struct {
	Year  int `json:"year,omitempty" jsonschema:"会计年度，与 month 同给或同省"`
	Month int `json:"month,omitempty" jsonschema:"会计月份 1-12，与 year 同给或同省"`
}

// validateAttachmentOwner 校验附件归属：类型必须在白名单内、
// id 必须是正整数。上传/删除/列表三处共用 —— 归属写错了，
// 附件就挂到另一张单据上，比报错难查得多。
func validateAttachmentOwner(ownerType string, ownerID int64) error {
	if err := enumIn(ownerType, attachmentOwnerTypes, "ownerType"); err != nil {
		return err
	}
	if strings.TrimSpace(ownerType) == "" {
		return &service.InvalidInput{
			Msg: "ownerType 必填：voucher|invoice|expense_claim|expense_item"}
	}
	if ownerID <= 0 {
		return &service.InvalidInput{Msg: "ownerId 必须是正整数"}
	}
	return nil
}

// attachmentOut 是附件对外的形状：hash/name/size/mime/missing。
// ★ 故意不带 Path —— 宿主机绝对路径对远程客户端没用，
// 还会把服务端磁盘布局泄出去。
func attachmentOut(a service.AttachmentInfo) map[string]any {
	return map[string]any{
		"hash": a.Hash, "name": a.Name, "size": a.Size,
		"mime": a.MIME, "missing": a.Missing,
	}
}

// enumIn 校验可选枚举入参：空值放行（表示不限），
// 集合外值报 invalid_params。
func enumIn(v string, allowed map[string]struct{}, field string) error {
	if v == "" {
		return nil
	}
	if _, ok := allowed[v]; ok {
		return nil
	}
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	return &service.InvalidInput{
		Msg: fmt.Sprintf("%s 只接受 %s，收到 %q", field, strings.Join(keys, "|"), v)}
}
