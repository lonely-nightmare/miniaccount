package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	Direction string `json:"direction,omitempty" jsonschema:"in|out，空表示全部"`
	Status    string `json:"status,omitempty" jsonschema:"pending|verified|booked|voided，空表示全部"`
	From      string `json:"from,omitempty" jsonschema:"开票日期起 YYYY-MM-DD"`
	To        string `json:"to,omitempty" jsonschema:"开票日期止 YYYY-MM-DD"`
	pageIn
}

type listBankFlowsIn struct {
	Status string `json:"status,omitempty" jsonschema:"unmatched|matched|booked 等，空表示全部"`
	From   string `json:"from,omitempty" jsonschema:"日期起 YYYY-MM-DD"`
	To     string `json:"to,omitempty" jsonschema:"日期止 YYYY-MM-DD"`
	pageIn
}

// registerTools 注册 14 读 + 6 写。
func (s *Server) registerTools() {
	mcp.AddTool(s.core, &mcp.Tool{
		Name:        "book_info",
		Description: "账套信息：名称、公司、纳税人身份、会计期间列表",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ bookInfoIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listAccountsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listPeriodsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryVouchersIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getVoucherIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getLedgerIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in periodIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in balanceSheetIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in periodIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listContactsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listDepartmentsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listEmployeesIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listInvoicesIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.Invoices(ctx, service.InvoiceQuery{
				Direction: in.Direction, Status: in.Status, From: in.From, To: in.To,
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listBankFlowsIn) (*mcp.CallToolResult, any, error) {
		return s.run(ctx, func(ctx context.Context, svc *service.Service) (any, error) {
			v, err := svc.BankFlows(ctx, service.BankFlowQuery{
				Status: in.Status, From: in.From, To: in.To,
			})
			if err != nil {
				return nil, err
			}
			return pageOf(yuanize(v), in.pageIn), nil
		})
	})

	if !s.readonly {
		s.registerWriteTools()
	}
}
