package service_test

import (
	"context"
	"strings"
	"testing"

	"miniaccount/internal/service"
)

// 三态补丁：缺席保持、null 清空、值覆盖。
func TestPatchContactTriState(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)

	id, err := svc.SaveContact(ctx, service.ContactInput{
		Kind: "customer", Name: "原单位", TaxNo: "9133", BankName: "工行", Enabled: true,
	})
	if err != nil {
		t.Fatalf("建档失败: %v", err)
	}

	// 只给 phone：其余字段必须原样保留。
	if _, err := svc.PatchContact(ctx, id, service.ContactPatch{
		Phone: service.Field[string]{Set: true, Value: "13800000000"},
	}); err != nil {
		t.Fatalf("部分更新失败: %v", err)
	}
	list, _ := svc.Contacts(ctx, "")
	var c *service.Contact
	for i := range list {
		if list[i].ID == id {
			c = &list[i]
		}
	}
	if c == nil || c.Phone != "13800000000" || c.TaxNo != "9133" || c.BankName != "工行" {
		t.Fatalf("缺席字段被改动: %+v", c)
	}

	// 显式清空 taxNo + 改名。
	if _, err := svc.PatchContact(ctx, id, service.ContactPatch{
		TaxNo: service.Field[string]{Set: true, Null: true},
		Name:  service.Field[string]{Set: true, Value: "新单位"},
	}); err != nil {
		t.Fatalf("清空/设值失败: %v", err)
	}
	list, _ = svc.Contacts(ctx, "")
	for i := range list {
		if list[i].ID == id {
			c = &list[i]
		}
	}
	if c.TaxNo != "" || c.Name != "新单位" {
		t.Fatalf("清空/设值未生效: %+v", c)
	}
}

// 员工补丁只走白名单字段；且不允许用来建人/改薪酬基数。
func TestPatchEmployeeWhitelist(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t, 3)

	eid, err := svc.SaveEmployee(ctx, service.EmployeeInput{
		Name: "张三", BaseSalary: 800000,
	})
	if err != nil {
		t.Fatalf("建档失败: %v", err)
	}

	// 白名单字段生效。
	if err := svc.PatchEmployee(ctx, eid, service.EmployeePatch{
		Phone:    service.Field[string]{Set: true, Value: "13900000000"},
		BankName: service.Field[string]{Set: true, Value: "招行"},
	}); err != nil {
		t.Fatalf("员工补丁失败: %v", err)
	}
	emps, _ := svc.Employees(ctx, false)
	var found bool
	for _, e := range emps {
		if e.ID == eid {
			found = true
			if e.Phone != "13900000000" || e.BankName != "招行" {
				t.Fatalf("员工补丁未生效: %+v", e)
			}
			if e.BaseSalary != 800000 {
				t.Fatalf("补丁不应改动薪酬基数: %d", e.BaseSalary)
			}
		}
	}
	if !found {
		t.Fatal("员工不见了")
	}

	// 不存在的员工必须报错，不能静默建人。
	err = svc.PatchEmployee(ctx, 99999, service.EmployeePatch{
		Phone: service.Field[string]{Set: true, Value: "1"},
	})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("对不存在的员工打补丁应报错: %v", err)
	}
}
