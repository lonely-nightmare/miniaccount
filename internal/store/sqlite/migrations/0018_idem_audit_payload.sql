-- 幂等审计补记要「原样回放首次创建的内容」且并发不双写
-- （文档 §2.5 复查）。
--
-- audit_payload：与凭证同事务落库时就把审计明细 JSON 存下来。
--   此前 failed 态补记从「当前凭证」重建明细 —— 凭证落库后被
--   改过的话，补记会把改后内容记成初始创建，日志失真。
-- repair_claim_at：补记认领时间戳。CAS 只允许
--   failed → repairing 或「过期 repairing → repairing」——
--   并发重试只有一个赢家写日志；补记途中崩溃留下的 repairing
--   过了认领窗口后能被下一个重试自愈。
ALTER TABLE mcp_idempotency ADD COLUMN audit_payload TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_idempotency ADD COLUMN repair_claim_at TEXT NOT NULL DEFAULT '';
