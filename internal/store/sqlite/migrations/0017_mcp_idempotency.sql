-- 幂等键（文档 §2.5）：create_draft_voucher 可选 clientRequestId。
--
-- 键与凭证同事务落库（audit_state=pending）；提交后审计写完再
-- 更新 recorded/failed——重试时凭它证明「原操作到底留没留痕」，
-- 新 context/重启都不影响。
CREATE TABLE IF NOT EXISTS mcp_idempotency (
  request_id TEXT PRIMARY KEY,
  -- 规范化请求（字段排序后 JSON）的摘要：同键不同内容
  -- 返回 idempotency_conflict，不得拿旧凭证冒充成功。
  request_hash TEXT NOT NULL,
  voucher_id INTEGER NOT NULL,
  -- 首次落库时凭证的 updated_at 原文：created_at/updated_at
  -- 是两次 nowString()，不能靠相等判断「之后被改过」。
  initial_voucher_updated_at TEXT NOT NULL,
  audit_state TEXT NOT NULL DEFAULT 'pending',
  audit_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
