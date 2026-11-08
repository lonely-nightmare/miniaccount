package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IdemRow 是 mcp_idempotency 的一行（文档 §2.5）。
type IdemRow struct {
	RequestID        string
	RequestHash      string
	VoucherID        int64
	InitialUpdatedAt string
	AuditState       string // pending | recorded | failed | repairing
	AuditError       string
	// AuditPayload 是创建审计明细 JSON（{summary, detail}）。
	// 与凭证同事务落库：failed 态补记回放它，不读当前凭证——
	// 凭证落库后被改过的话，「当前内容」会把修改记成初始创建。
	AuditPayload string
	CreatedAt    string
}

// MCPIdemRepo 幂等键仓储。
type MCPIdemRepo struct{ db *DB }

// MCPIdem 返回幂等键仓储。
func (db *DB) MCPIdem() *MCPIdemRepo { return &MCPIdemRepo{db: db} }

// Get 按键取行；不存在返回 (nil, nil)。
func (r *MCPIdemRepo) Get(ctx context.Context, requestID string) (*IdemRow, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT request_id, request_hash, voucher_id, initial_voucher_updated_at,
			audit_state, audit_error, audit_payload, created_at
		FROM mcp_idempotency WHERE request_id = ?`, requestID)
	var o IdemRow
	err := row.Scan(&o.RequestID, &o.RequestHash, &o.VoucherID,
		&o.InitialUpdatedAt, &o.AuditState, &o.AuditError,
		&o.AuditPayload, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// InsertTx 在调用方给定的账套事务内插幂等行（与凭证同生共死）。
// 并发同键由 request_id 主键裁决：后到者整个事务回滚。
func (r *MCPIdemRepo) InsertTx(ctx context.Context, tx *Tx, row IdemRow) error {
	if row.AuditState == "" {
		row.AuditState = "pending"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO mcp_idempotency
			(request_id, request_hash, voucher_id, initial_voucher_updated_at,
			 audit_state, audit_error, audit_payload, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		row.RequestID, row.RequestHash, row.VoucherID, row.InitialUpdatedAt,
		row.AuditState, row.AuditError, row.AuditPayload,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// SetAuditState 提交后更新审计留痕状态；写不进就只留 pending——
// pending 的语义本来就是「提交后崩溃，未确认已留痕」。
func (r *MCPIdemRepo) SetAuditState(ctx context.Context, requestID, state, errText string) {
	_, _ = r.db.sql.ExecContext(ctx, `
		UPDATE mcp_idempotency SET audit_state = ?, audit_error = ?
		WHERE request_id = ?`, state, errText, requestID)
}

// repairClaimStale 是补记认领的过期窗口：repairing 状态停留超过
// 它就视为「上次补记途中崩溃」，允许下一个重试重新认领；
// 窗口内的 repairing 是「别人正在补」，不许再进（否则并发重试
// 会各写一条创建日志）。补记本身毫秒级完成，60s 足够保守。
const repairClaimStale = 60 * time.Second

// ClaimAuditRepair 认领一次审计补记：failed → repairing，
// 或「认领已过期」的 repairing → repairing 的 CAS。
// 认领不到（并发重试已抢先、或状态不是可补记的）返回 false。
func (r *MCPIdemRepo) ClaimAuditRepair(ctx context.Context, requestID string) (bool, error) {
	now := time.Now().UTC()
	staleBefore := now.Add(-repairClaimStale).Format(time.RFC3339Nano)
	res, err := r.db.sql.ExecContext(ctx, `
		UPDATE mcp_idempotency
		SET audit_state = 'repairing', repair_claim_at = ?
		WHERE request_id = ? AND (
			audit_state = 'failed' OR
			(audit_state = 'repairing' AND repair_claim_at < ?)
		)`, now.Format(time.RFC3339Nano), requestID, staleBefore)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
