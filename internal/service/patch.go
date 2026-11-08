package service

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"miniaccount/internal/domain/audit"
)

// ---------------------------------------------------------------------------
// 三态补丁字段
// ---------------------------------------------------------------------------
//
// MCP 工具与「部分更新」需要区分三种输入，单靠 Go 零值做不到：
//   - Set=false：请求里没这个字段 → 保持原值；
//   - Set=true, Null=true：请求显式给 null → 清空；
//   - Set=true, Null=false：设置新值。
//
// 适配层（mcpserver）用 map[string]json.RawMessage 判定缺席与否后
// 构造这些字段；服务层只看 Set/Null/Value。

// Field 是三态补丁字段。
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// Str 把补丁应用到字符串字段。
func (f Field[T]) apply(cur *T) {
	if !f.Set {
		return
	}
	if f.Null {
		var zero T
		*cur = zero
		return
	}
	*cur = f.Value
}

// ---------------------------------------------------------------------------
// 往来单位
// ---------------------------------------------------------------------------

// ContactPatch 是往来单位的部分更新；id=0 表示新建（未提供的字段取默认）。
type ContactPatch struct {
	Kind          Field[string]
	Name          Field[string]
	ShortName     Field[string]
	TaxNo         Field[string]
	BankName      Field[string]
	BankAccount   Field[string]
	ContactPerson Field[string]
	Phone         Field[string]
	Address       Field[string]
	Enabled       Field[bool]
	Remark        Field[string]
}

// PatchContact 建/改往来单位：读旧→合并→校验→写 全在同一事务。
func (s *Service) PatchContact(ctx context.Context, id int64, p ContactPatch) (out int64, err error) {
	defer func() {
		if err == nil {
			s.recordAudit(ctx, AuditEvent{
				Action:  audit.ActionContactSave,
				Summary: "维护往来单位（MCP 部分更新）",
				Entity:  "contact", EntityID: strconv.FormatInt(out, 10),
			})
		}
	}()
	err = s.db.WithTx(ctx, func(tx *sqliteTx) error {
		in := ContactInput{ID: id, Enabled: true}
		if id != 0 {
			var en int
			row := tx.QueryRow(ctx, `SELECT kind, name, COALESCE(short_name,''), COALESCE(tax_no,''),
				COALESCE(bank_name,''), COALESCE(bank_account,''), COALESCE(contact_person,''),
				COALESCE(phone,''), COALESCE(address,''), is_enabled, COALESCE(remark,'')
				FROM contact WHERE id = ?`, id)
			if e := row.Scan(&in.Kind, &in.Name, &in.ShortName, &in.TaxNo, &in.BankName,
				&in.BankAccount, &in.ContactPerson, &in.Phone, &in.Address, &en, &in.Remark); e != nil {
				return fmt.Errorf("往来单位 id=%d 不存在: %w", id, e)
			}
			in.Enabled = en != 0
		}
		p.Kind.apply(&in.Kind)
		p.Name.apply(&in.Name)
		p.ShortName.apply(&in.ShortName)
		p.TaxNo.apply(&in.TaxNo)
		p.BankName.apply(&in.BankName)
		p.BankAccount.apply(&in.BankAccount)
		p.ContactPerson.apply(&in.ContactPerson)
		p.Phone.apply(&in.Phone)
		p.Address.apply(&in.Address)
		p.Enabled.apply(&in.Enabled)
		p.Remark.apply(&in.Remark)
		var e error
		out, e = s.saveContactTx(ctx, tx, in)
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 部门
// ---------------------------------------------------------------------------

// DepartmentPatch 是部门的部分更新；id=0 表示新建。
type DepartmentPatch struct {
	Code    Field[string]
	Name    Field[string]
	Parent  Field[int64] // Null 表示「顶层部门」
	Enabled Field[bool]
	Remark  Field[string]
}

// PatchDepartment 建/改部门，读改写同事务。
func (s *Service) PatchDepartment(ctx context.Context, id int64, p DepartmentPatch) (out int64, err error) {
	var d Department
	defer func() {
		if err == nil {
			s.recordAudit(ctx, AuditEvent{
				Action:  audit.ActionDepartmentSave,
				Summary: fmt.Sprintf("维护部门 %s（MCP 部分更新）", d.Name),
				Entity:  "department", EntityID: strconv.FormatInt(out, 10),
			})
		}
	}()
	err = s.db.WithTx(ctx, func(tx *sqliteTx) error {
		d = Department{ID: id, Enabled: true}
		if id != 0 {
			var en int
			row := tx.QueryRow(ctx, `SELECT code, name, parent_id, is_enabled,
				COALESCE(remark,'') FROM department WHERE id = ?`, id)
			var parent sql.NullInt64
			if e := row.Scan(&d.Code, &d.Name, &parent, &en, &d.Remark); e != nil {
				return fmt.Errorf("部门 id=%d 不存在: %w", id, e)
			}
			d.Enabled = en != 0
			if parent.Valid {
				v := parent.Int64
				d.ParentID = &v
			}
		}
		p.Code.apply(&d.Code)
		p.Name.apply(&d.Name)
		p.Enabled.apply(&d.Enabled)
		p.Remark.apply(&d.Remark)
		if p.Parent.Set {
			if p.Parent.Null {
				d.ParentID = nil
			} else {
				v := p.Parent.Value
				d.ParentID = &v
			}
		}
		var e error
		out, e = s.saveDepartmentTx(ctx, tx, d)
		return e
	})
	return out, err
}

// ---------------------------------------------------------------------------
// 员工（仅修改，白名单）
// ---------------------------------------------------------------------------

// EmployeePatch 只含允许部分更新的字段。薪酬基数/部门/离职/启用
// 有各自的业务动作（调薪、调动、离职、停用），不走补丁，
// 否则历史工资单会被静默改写口径。
type EmployeePatch struct {
	Name               Field[string]
	Phone              Field[string]
	IDCard             Field[string]
	BankName           Field[string]
	BankAccount        Field[string]
	ExpenseAccountCode Field[string]
	CompanyAccountCode Field[string]
	Remark             Field[string]
}

// PatchEmployee 部分修改员工；仅更新，不建人。
func (s *Service) PatchEmployee(ctx context.Context, id int64, p EmployeePatch) (err error) {
	defer func() {
		if err == nil {
			s.recordAudit(ctx, AuditEvent{
				Action:  audit.ActionEmployeeSave,
				Summary: "维护员工（MCP 部分更新）",
				Entity:  "employee", EntityID: strconv.FormatInt(id, 10),
			})
		}
	}()
	return s.db.WithTx(ctx, func(tx *sqliteTx) error {
		e, e2 := s.db.Payroll().EmployeeByIDTx(ctx, tx, id)
		if e2 != nil {
			return e2
		}
		p.Name.apply(&e.Name)
		p.Phone.apply(&e.Phone)
		p.IDCard.apply(&e.IDCard)
		p.BankName.apply(&e.BankName)
		p.BankAccount.apply(&e.BankAccount)
		p.ExpenseAccountCode.apply(&e.ExpenseAccountCode)
		p.CompanyAccountCode.apply(&e.CompanyAccountCode)
		p.Remark.apply(&e.Remark)
		_, e2 = s.db.Payroll().UpsertEmployeeTx(ctx, tx, e)
		return e2
	})
}
