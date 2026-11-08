-- 0016_invoice_post_params.sql
-- 发票生成凭证的两个参数落库：费用归集科目与部门。
--
-- 原来表单收集了 expense_account/dept_id 却在保存时丢弃
-- （service.SaveInvoice 里 _ = in.DeptID），过账时只能靠调用方
-- 再传一次 —— 而列表页的「生成凭证」按钮根本拿不到当时填的值，
-- 默认科目 560206 又要求部门辅助核算，结果是被过账校验拒绝。
--
-- 存量行 expense_account=''、dept_id=NULL：回落到「默认科目 +
-- 要求部门则报错」的旧行为，语义不变。

ALTER TABLE invoice ADD COLUMN expense_account TEXT NOT NULL DEFAULT '';
ALTER TABLE invoice ADD COLUMN dept_id INTEGER REFERENCES department(id);
