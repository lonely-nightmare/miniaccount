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
	AuditState       string // pending | recorded | failed
	AuditError       string
	CreatedAt        string
}

// MCPIdemRepo 幂等键仓储。
type MCPIdemRepo struct{ db *DB }

// MCPIdem 返回幂等键仓储。
func (db *DB) MCPIdem() *MCPIdemRepo { return &MCPIdemRepo{db: db} }

// Get 按键取行；不存在返回 (nil, nil)。
func (r *MCPIdemRepo) Get(ctx context.Context, requestID string) (*IdemRow, error) {
	row := r.db.sql.QueryRowContext(ctx, `
		SELECT request_id, request_hash, voucher_id, initial_voucher_updated_at,
			audit_state, audit_error, created_at
		FROM mcp_idempotency WHERE request_id = ?`, requestID)
	var o IdemRow
	err := row.Scan(&o.RequestID, &o.RequestHash, &o.VoucherID,
		&o.InitialUpdatedAt, &o.AuditState, &o.AuditError, &o.CreatedAt)
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
			 audit_state, audit_error, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		row.RequestID, row.RequestHash, row.VoucherID, row.InitialUpdatedAt,
		row.AuditState, row.AuditError,
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
