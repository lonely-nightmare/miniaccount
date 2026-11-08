package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"miniaccount/internal/domain/money"
	"miniaccount/internal/service"
)

// ---------------------------------------------------------------------------
// 写工具输入
// ---------------------------------------------------------------------------

type voucherLineIn struct {
	AccountCode string `json:"accountCode" jsonschema:"科目编码，如 660201"`
	Summary     string `json:"summary,omitempty" jsonschema:"分录摘要，留空沿用凭证摘要"`
	Debit       string `json:"debit,omitempty" jsonschema:"借方金额（元字符串，如 \"1234.56\"）"`
	Credit      string `json:"credit,omitempty" jsonschema:"贷方金额（元字符串）"`
	ContactID   *int64 `json:"contactId,omitempty" jsonschema:"往来单位 id（科目要求辅助核算时必填）"`
	EmployeeID  *int64 `json:"employeeId,omitempty"`
	DeptID      *int64 `json:"deptId,omitempty"`
	ProjectID   *int64 `json:"projectId,omitempty"`
}

type createDraftVoucherIn struct {
	// Word 是凭证字：记|收|付|转，缺省记。
	// 不带这个字段时 MCP 建的凭证全是「记」——付款凭证
	// 的凭证字就写错了。
	Word            string          `json:"word,omitempty" jsonschema:"凭证字 记|收|付|转，缺省 记"`
	Date            string          `json:"date" jsonschema:"记账日期 YYYY-MM-DD"`
	Remark          string          `json:"remark,omitempty" jsonschema:"凭证摘要"`
	AttachCount     int             `json:"attachCount,omitempty" jsonschema:"附单据数"`
	Lines           []voucherLineIn `json:"lines" jsonschema:"分录，至少两行且借贷平衡"`
	ClientRequestID string          `json:"clientRequestId,omitempty" jsonschema:"幂等键：同键同内容重试返回原凭证"`
}

type updateDraftVoucherIn struct {
	ID                int64           `json:"id" jsonschema:"凭证 id"`
	ExpectedUpdatedAt string          `json:"expectedUpdatedAt" jsonschema:"读取时看到的 updatedAt（乐观锁）"`
	Word              *string         `json:"word,omitempty" jsonschema:"凭证字 记|收|付|转"`
	Date              *string         `json:"date,omitempty"`
	Remark            *string         `json:"remark,omitempty"`
	AttachCount       *int            `json:"attachCount,omitempty"`
	Lines             []voucherLineIn `json:"lines,omitempty" jsonschema:"给出时整体替换分录"`
}

type deleteDraftVoucherIn struct {
	ID                int64  `json:"id" jsonschema:"凭证 id"`
	ExpectedUpdatedAt string `json:"expectedUpdatedAt" jsonschema:"读取时看到的 updatedAt（乐观锁）"`
}

// 档案类写工具用 map 输入 —— 需要区分「缺席 / null / 有值」三态，
// 结构体输入做不到（缺席和零值看起来一样）。
// 三态合并规则在服务层 patch.go。

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

func (s *Server) registerWriteTools() {
	// create_draft_voucher —— 全量校验 + 幂等键。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "create_draft_voucher",
		Description: "创建凭证**草稿**（不会过账）。走与界面相同的全部校验；" +
			"带 clientRequestId 时同键重试返回原凭证。过账请让人在界面确认后操作。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createDraftVoucherIn) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		vin, err := buildVoucherInput(in.Lines, in.Date, in.Remark, in.AttachCount)
		if err != nil {
			return toolError(err)
		}
		vin.Word = in.Word
		vin.CreatedBy = s.operator
		hash := hashCreateRequest(vin)
		var voucherID int64
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			d, outcome, err := svc.CreateDraftVoucherIdempotent(ctx, *vin, in.ClientRequestID, hash)
			if err != nil {
				return nil, err
			}
			voucherID = d.ID
			return voucherResult(d, outcome, warn), nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		s.changed("voucher", voucherID)
		return nil, out, nil
	})

	// update_draft_voucher —— 仅草稿 + 乐观锁。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "update_draft_voucher",
		Description: "修改**草稿**凭证；必须带读取时的 updatedAt。" +
			"已过账凭证不能改（要改请走红字冲销，MCP 不提供）。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateDraftVoucherIn) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		if strings.TrimSpace(in.ExpectedUpdatedAt) == "" {
			return toolError(&service.InvalidInput{
				Msg: "expectedUpdatedAt 不能为空：先 get_voucher 取 updatedAt 再回传"})
		}
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			d, err := svc.Voucher(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			if d.Status != "draft" {
				return nil, &service.InvalidInput{Msg: "只能修改草稿凭证"}
			}
			vin := service.VoucherInput{
				// Word 必须回填：留空在服务层会套用默认
				// 「记」字——只改摘要就把「付」字凭证改成「记」。
				ID: in.ID, Word: d.Word, Date: d.Date, Remark: d.Remark,
				AttachCount: d.AttachCount, ExpectedUpdatedAt: in.ExpectedUpdatedAt,
				CreatedBy: d.CreatedBy,
			}
			if in.Word != nil {
				vin.Word = *in.Word
			}
			if in.Date != nil {
				vin.Date = *in.Date
			}
			if in.Remark != nil {
				vin.Remark = *in.Remark
			}
			if in.AttachCount != nil {
				vin.AttachCount = *in.AttachCount
			}
			if in.Lines != nil {
				lines, err := buildLines(in.Lines, vin.Remark)
				if err != nil {
					return nil, err
				}
				vin.Lines = lines
			} else {
				for _, l := range d.Lines {
					vin.Lines = append(vin.Lines, service.VoucherLineInput{
						AccountCode: l.AccountCode, Summary: l.Summary,
						Debit: l.Debit, Credit: l.Credit,
						ContactID: l.ContactID, EmployeeID: l.EmployeeID,
						DeptID: l.DeptID, ProjectID: l.ProjectID,
					})
				}
			}
			// 修改保留原来源 —— MCP 改一张银行流水生成的凭证，
			// 它的来源仍应是 bank，不是 mcp。service 层按此落库。
			out, err := svc.SaveVoucher(ctx, vin)
			if err != nil {
				return nil, err
			}
			return voucherResult(out, nil, warn), nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		s.changed("voucher", in.ID)
		return nil, out, nil
	})

	// delete_draft_voucher —— 仅草稿 + 乐观锁。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "delete_draft_voucher",
		Description: "删除**草稿**凭证；必须带读取时的 updatedAt。" +
			"已过账凭证不能删。工资单生成的凭证请从工资模块处理。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in deleteDraftVoucherIn) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		if strings.TrimSpace(in.ExpectedUpdatedAt) == "" {
			return toolError(&service.InvalidInput{
				Msg: "expectedUpdatedAt 不能为空：先 get_voucher 取 updatedAt 再回传"})
		}
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			d, err := svc.Voucher(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			if d.Status != "draft" {
				return nil, &service.InvalidInput{Msg: "只能删除草稿凭证"}
			}
			if err := svc.DeleteVoucher(ctx, in.ID, in.ExpectedUpdatedAt); err != nil {
				return nil, err
			}
			return map[string]any{
				"deleted": true, "id": in.ID,
				"auditWarnings": warn.All(),
			}, nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		s.changed("voucher", in.ID)
		return nil, out, nil
	})

	// upsert_contact —— 三态部分更新。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "upsert_contact",
		Description: "新建或部分更新往来单位。id 缺席=新建；字段缺席=保持原值，" +
			"字段为 null=清空，字段有值=设置。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		in, err := decodeMapArgs(req)
		if err != nil {
			return toolError(&service.InvalidInput{Msg: "参数不是合法 JSON 对象：" + err.Error()})
		}
		if err := checkPatchKeys(in, contactPatchKeys, "upsert_contact"); err != nil {
			return toolError(err)
		}
		var id int64
		if v, ok := in["id"]; ok && v != nil {
			var ierr error
			id, ierr = idFromJSON(v)
			if ierr != nil {
				return toolError(ierr)
			}
			delete(in, "id")
		}
		var pe patchErrs
		p := service.ContactPatch{
			Kind:          pe.patchStr(in, "kind"),
			Name:          pe.patchStr(in, "name"),
			ShortName:     pe.patchStr(in, "shortName"),
			TaxNo:         pe.patchStr(in, "taxNo"),
			BankName:      pe.patchStr(in, "bankName"),
			BankAccount:   pe.patchStr(in, "bankAccount"),
			ContactPerson: pe.patchStr(in, "contactPerson"),
			Phone:         pe.patchStr(in, "phone"),
			Address:       pe.patchStr(in, "address"),
			Enabled:       pe.patchBool(in, "enabled"),
			Remark:        pe.patchStr(in, "remark"),
		}
		if err := pe.err(); err != nil {
			return toolError(err)
		}
		// kind 原样落库（saveContactTx 只验名称非空）——
		// 枚举外的值会存成脏数据，在这层拦下。
		if k := p.Kind; k.Set && !k.Null {
			if err := enumIn(k.Value, contactKinds, "kind"); err != nil {
				return toolError(err)
			}
		}
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			newID, err := svc.PatchContact(ctx, id, p)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": newID, "auditWarnings": warn.All()}, nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		if m, ok := out.(map[string]any); ok {
			if vid, ok := m["id"].(int64); ok {
				s.changed("contact", vid)
			}
		}
		return nil, out, nil
	})

	// save_department —— 三态部分更新。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "save_department",
		Description: "新建或部分更新部门。id 缺席=新建；parentId=null 表示顶层部门。" +
			"三态规则同 upsert_contact。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		in, err := decodeMapArgs(req)
		if err != nil {
			return toolError(&service.InvalidInput{Msg: "参数不是合法 JSON 对象：" + err.Error()})
		}
		if err := checkPatchKeys(in, departmentPatchKeys, "save_department"); err != nil {
			return toolError(err)
		}
		var id int64
		if v, ok := in["id"]; ok && v != nil {
			var ierr error
			id, ierr = idFromJSON(v)
			if ierr != nil {
				return toolError(ierr)
			}
			delete(in, "id")
		}
		var pe patchErrs
		p := service.DepartmentPatch{
			Code:    pe.patchStr(in, "code"),
			Name:    pe.patchStr(in, "name"),
			Parent:  pe.patchInt(in, "parentId"),
			Enabled: pe.patchBool(in, "enabled"),
			Remark:  pe.patchStr(in, "remark"),
		}
		if err := pe.err(); err != nil {
			return toolError(err)
		}
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			newID, err := svc.PatchDepartment(ctx, id, p)
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": newID, "auditWarnings": warn.All()}, nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		if m, ok := out.(map[string]any); ok {
			if did, ok := m["id"].(int64); ok {
				s.changed("department", did)
			}
		}
		return nil, out, nil
	})

	// update_employee —— 白名单部分更新。
	mcp.AddTool(s.core, &mcp.Tool{
		Name: "update_employee",
		Description: "部分更新员工档案（仅修改，不建人）。可调字段：name/phone/idCard/" +
			"bankName/bankAccount/expenseAccountCode/companyAccountCode/remark。" +
			"薪酬基数、部门、离职、启用各有业务动作，不在此工具内。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		ctx, warn := s.callCtx(ctx)
		in, err := decodeMapArgs(req)
		if err != nil {
			return toolError(&service.InvalidInput{Msg: "参数不是合法 JSON 对象：" + err.Error()})
		}
		v, ok := in["id"]
		if !ok || v == nil {
			return toolError(&service.InvalidInput{Msg: "update_employee 必须提供员工 id"})
		}
		// 白名单外的键一律拒绝 —— 传 baseSalary 被静默忽略
		// 比报错危险：用户以为改了薪酬，实际一个字没动。
		// 验收口径：「传 baseSalary/enabled 被拒」。
		if err := checkPatchKeys(in, employeePatchKeys, "update_employee"); err != nil {
			return toolError(err)
		}
		eid, ierr := idFromJSON(v)
		if ierr != nil {
			return toolError(ierr)
		}
		var pe patchErrs
		p := service.EmployeePatch{
			Name:               pe.patchStr(in, "name"),
			Phone:              pe.patchStr(in, "phone"),
			IDCard:             pe.patchStr(in, "idCard"),
			BankName:           pe.patchStr(in, "bankName"),
			BankAccount:        pe.patchStr(in, "bankAccount"),
			ExpenseAccountCode: pe.patchStr(in, "expenseAccountCode"),
			CompanyAccountCode: pe.patchStr(in, "companyAccountCode"),
			Remark:             pe.patchStr(in, "remark"),
		}
		if err := pe.err(); err != nil {
			return toolError(err)
		}
		res, out, err := s.run(ctx, req, func(ctx context.Context, svc *service.Service) (any, error) {
			if err := svc.PatchEmployee(ctx, eid, p); err != nil {
				return nil, err
			}
			return map[string]any{"id": eid, "auditWarnings": warn.All()}, nil
		})
		if res != nil || err != nil {
			return res, out, err
		}
		s.changed("employee", eid)
		return nil, out, nil
	})
}

// ---------------------------------------------------------------------------
// 装配辅助
// ---------------------------------------------------------------------------

// buildVoucherInput 把工具输入拼成服务层输入；金额元字符串在这里解析成分。
func buildVoucherInput(lines []voucherLineIn, date, remark string, attach int) (*service.VoucherInput, error) {
	ls, err := buildLines(lines, remark)
	if err != nil {
		return nil, err
	}
	return &service.VoucherInput{
		Date: date, Remark: remark, AttachCount: attach, Lines: ls,
	}, nil
}

func buildLines(in []voucherLineIn, fallbackSummary string) ([]service.VoucherLineInput, error) {
	if len(in) == 0 {
		return nil, &service.InvalidInput{Msg: "至少需要一条分录（实际至少要两行且借贷平衡）"}
	}
	out := make([]service.VoucherLineInput, 0, len(in))
	for i, l := range in {
		var d, c money.Money
		if l.Debit != "" {
			var err error
			if d, err = parseYuan(l.Debit); err != nil {
				return nil, &service.InvalidInput{Msg: fmt.Sprintf("第 %d 行借方金额: %v", i+1, err)}
			}
		}
		if l.Credit != "" {
			var err error
			if c, err = parseYuan(l.Credit); err != nil {
				return nil, &service.InvalidInput{Msg: fmt.Sprintf("第 %d 行贷方金额: %v", i+1, err)}
			}
		}
		summary := l.Summary
		if summary == "" {
			summary = fallbackSummary // 服务层要求分录摘要非空，工具契约允许留空沿用凭证摘要
		}
		out = append(out, service.VoucherLineInput{
			AccountCode: l.AccountCode, Summary: summary,
			Debit: d, Credit: c,
			ContactID: l.ContactID, EmployeeID: l.EmployeeID,
			DeptID: l.DeptID, ProjectID: l.ProjectID,
		})
	}
	return out, nil
}

// hashCreateRequest 对创建请求做内容指纹；同键不同内容据此识别。
func hashCreateRequest(in *service.VoucherInput) string {
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// voucherOut 输出凭证：金额字段换成元字符串（契约：不暴露分）。
func voucherOut(d *service.VoucherDetail) any {
	return map[string]any{
		"id": d.ID, "no": d.No, "word": d.Word, "date": d.Date,
		"remark": d.Remark, "status": d.Status, "statusLabel": d.StatusLabel,
		"attachCount": d.AttachCount, "source": d.Source,
		"createdBy": d.CreatedBy, "postedBy": d.PostedBy, "postedAt": d.PostedAt,
		"reversesNo": d.ReversesNo, "voidedByNo": d.VoidedByNo,
		"lines":       voucherLinesOut(d),
		"totalDebit":  yuanStr(d.TotalDebit),
		"totalCredit": yuanStr(d.TotalCredit),
		"balanced":    d.Balanced,
		"updatedAt":   d.UpdatedAt,
	}
}

func voucherLinesOut(d *service.VoucherDetail) []map[string]any {
	out := make([]map[string]any, 0, len(d.Lines))
	for _, l := range d.Lines {
		out = append(out, map[string]any{
			"lineNo": l.LineNo, "accountCode": l.AccountCode,
			"accountName": l.AccountName, "summary": l.Summary,
			"debit": yuanStr(l.Debit), "credit": yuanStr(l.Credit),
			"auxDesc":   l.AuxDesc,
			"contactId": l.ContactID, "employeeId": l.EmployeeID,
			"deptId": l.DeptID, "projectId": l.ProjectID,
		})
	}
	return out
}

// voucherResult 是写工具的统一返回形。
func voucherResult(d *service.VoucherDetail, outcome *service.IdemOutcome,
	warn *service.CallWarnings) map[string]any {
	res := map[string]any{"voucher": voucherOut(d)}
	if outcome != nil {
		res["idempotent"] = outcome.Idempotent
		res["modifiedSince"] = outcome.ModifiedSince
		if outcome.AuditState != "" {
			res["auditState"] = outcome.AuditState
		}
		if outcome.AuditError != "" {
			res["auditError"] = outcome.AuditError
		}
	}
	if w := warn.All(); len(w) > 0 {
		res["auditWarnings"] = w
	}
	return res
}

// 三个 map 输入写工具的完整可写键集合 —— 集合外的键一律
// 拒绝：打错字段名被静默忽略，比报错危险（用户以为改了，
// 实际一个字没动）。
var contactPatchKeys = map[string]struct{}{
	"kind": {}, "name": {}, "shortName": {}, "taxNo": {},
	"bankName": {}, "bankAccount": {}, "contactPerson": {},
	"phone": {}, "address": {}, "enabled": {}, "remark": {},
}

var departmentPatchKeys = map[string]struct{}{
	"code": {}, "name": {}, "parentId": {}, "enabled": {}, "remark": {},
}

// employeePatchKeys 是 update_employee 的完整可写键集合
// （服务层 EmployeePatch 的字段名）。
var employeePatchKeys = map[string]struct{}{
	"name": {}, "phone": {}, "idCard": {}, "bankName": {},
	"bankAccount": {}, "expenseAccountCode": {},
	"companyAccountCode": {}, "remark": {},
}

// checkPatchKeys 校验 map 输入只含白名单键（id 始终允许）。
func checkPatchKeys(in map[string]any, allowed map[string]struct{}, tool string) error {
	for k := range in {
		if k == "id" {
			continue
		}
		if _, ok := allowed[k]; !ok {
			return &service.InvalidInput{
				Msg: fmt.Sprintf("字段 %q 不在 %s 白名单内", k, tool)}
		}
	}
	return nil
}

// patchErrs 收集补丁字段的类型错误。
// 错误类型（如对 remark 发数字）以前会静默当零值清空字段 ——
// 现在聚合成 invalid_params 返回，字段一个都不动。
type patchErrs []string

func (e *patchErrs) fail(key, want string, got any) {
	*e = append(*e, fmt.Sprintf("%s 应为 %s 或 null，收到 %v", key, want, got))
}

func (e *patchErrs) err() error {
	if len(*e) == 0 {
		return nil
	}
	return &service.InvalidInput{Msg: "字段类型错误：" + strings.Join(*e, "；")}
}

// patchStr / patchBool / patchInt 把 map 输入解成三态 Field。
func (e *patchErrs) patchStr(in map[string]any, key string) service.Field[string] {
	v, ok := in[key]
	if !ok {
		return service.Field[string]{}
	}
	if v == nil {
		return service.Field[string]{Set: true, Null: true}
	}
	s, ok := v.(string)
	if !ok {
		e.fail(key, "字符串", v)
		return service.Field[string]{}
	}
	return service.Field[string]{Set: true, Value: s}
}

func (e *patchErrs) patchBool(in map[string]any, key string) service.Field[bool] {
	v, ok := in[key]
	if !ok {
		return service.Field[bool]{}
	}
	if v == nil {
		return service.Field[bool]{Set: true, Null: true}
	}
	b, ok := v.(bool)
	if !ok {
		e.fail(key, "布尔", v)
		return service.Field[bool]{}
	}
	return service.Field[bool]{Set: true, Value: b}
}

func (e *patchErrs) patchInt(in map[string]any, key string) service.Field[int64] {
	v, ok := in[key]
	if !ok {
		return service.Field[int64]{}
	}
	if v == nil {
		return service.Field[int64]{Set: true, Null: true}
	}
	i, err := int64FromJSON(v)
	if err != nil {
		e.fail(key, "整数", v)
		return service.Field[int64]{}
	}
	return service.Field[int64]{Set: true, Value: i}
}

// decodeMapArgs 用 UseNumber 重解原始参数体。
// SDK 提前解好的 map[string]any 把数字全变 float64——
// >2^53 的 id 会被静默取整，{"id":9007199254740993} 落到
// 9007199254740992 那条记录上，改错档案还没法察觉。
func decodeMapArgs(req *mcp.CallToolRequest) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// int64FromJSON 把 JSON 数字解成 int64——json.Number 精确解析
// （UseNumber 解出的），float64 是测试直接构造输入的兜底。
// 小数、超界一律报错，不许截断。
func int64FromJSON(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case float64:
		if n != math.Trunc(n) || n > math.MaxInt64 || n < math.MinInt64 {
			return 0, fmt.Errorf("不是合法整数: %v", n)
		}
		return int64(n), nil
	default:
		return 0, fmt.Errorf("不是数字: %v", v)
	}
}

// idFromJSON 把 map 输入的 id 解成 int64 —— 非数字、带小数、
// 超界一律 invalid_params，不许悄悄落到别的记录上。
func idFromJSON(v any) (int64, error) {
	i, err := int64FromJSON(v)
	if err != nil || i <= 0 {
		return 0, &service.InvalidInput{Msg: fmt.Sprintf("id 必须是正整数，收到 %v", v)}
	}
	return i, nil
}
