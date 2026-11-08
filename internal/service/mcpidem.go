// create_draft_voucher 的幂等语义（文档 §2.5）。
//
// 键与凭证同一账套事务落库；提交后按审计成败写 audit_state ——
// 重试凭持久化状态回答「原操作到底留没留痕」。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"miniaccount/internal/domain/audit"
	"miniaccount/internal/domain/money"
	"miniaccount/internal/domain/voucher"
	"miniaccount/internal/store/sqlite"
)

// ErrIdempotencyConflict 同键不同内容：不得拿旧凭证冒充成功。
var ErrIdempotencyConflict = errors.New("idempotency_conflict：同一 clientRequestId 提交了不同内容")

// ErrIdempotentTargetDeleted 原凭证已删：不悄悄重建。
var ErrIdempotentTargetDeleted = errors.New("idempotent_target_deleted：该请求建过的凭证已被删除")

// IdemOutcome 是幂等命中的附带信息。
type IdemOutcome struct {
	// Idempotent 为真表示本次是重试命中，没有新建。
	Idempotent bool `json:"idempotent"`
	// ModifiedSince 为真表示原凭证在首次落库后被改过
	// （以 initial_voucher_updated_at 为基准）。
	ModifiedSince bool `json:"modifiedSince"`
	// AuditState 是原操作的审计留痕状态：recorded/pending/failed。
	// pending = 「提交后崩溃，未确认已留痕」；failed 的错误在 AuditError。
	AuditState string `json:"auditState,omitempty"`
	AuditError string `json:"auditError,omitempty"`
}

// CreateDraftVoucherIdempotent 按幂等键创建草稿凭证。
// requestID 为空时退化为普通 SaveVoucher。
func (s *Service) CreateDraftVoucherIdempotent(ctx context.Context,
	in VoucherInput, requestID, requestHash string) (*VoucherDetail, *IdemOutcome, error) {
	// MCP 创建的来源固定 mcp（凭证红线：外部建的单据要认得出）。
	in.Source = voucher.SourceMCP
	in.SourceID = nil

	if requestID == "" {
		d, err := s.SaveVoucher(ctx, in)
		return d, &IdemOutcome{}, err
	}

	// 快路径：键已存在 → 按持久化状态回答。
	if row, err := s.db.MCPIdem().Get(ctx, requestID); err == nil && row != nil {
		return s.resolveIdem(ctx, row, requestHash)
	}
	v, err := s.buildVoucher(ctx, in)
	if err != nil {
		return nil, nil, err
	}

	var vid int64
	var initStamp string
	err = s.db.WithTx(ctx, func(tx *sqlite.Tx) error {
		res, e := s.db.Vouchers().SaveDraftInTx(ctx, tx, sqlite.DraftInput{
			Voucher: v, CreatedBy: v.CreatedBy,
		})
		if e != nil {
			return e
		}
		vid = res.VoucherID
		// 落库时的 updated_at 原文：modifiedSince 的比较基准
		// （两次 nowString() 不相等，不能靠它们判断）。
		if e := tx.QueryRow(ctx,
			`SELECT updated_at FROM voucher WHERE id = ?`, vid).
			Scan(&initStamp); e != nil {
			return e
		}
		return s.db.MCPIdem().InsertTx(ctx, tx, sqlite.IdemRow{
			RequestID: requestID, RequestHash: requestHash,
			VoucherID: vid, InitialUpdatedAt: initStamp,
			AuditState: "pending",
		})
	})
	if err != nil {
		// 并发同键：主键裁决后到者回滚；读出先到者按持久化状态回答。
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			if row, gerr := s.db.MCPIdem().Get(ctx, requestID); gerr == nil && row != nil {
				return s.resolveIdem(ctx, row, requestHash)
			}
		}
		return nil, nil, err
	}

	// 提交后写审计；成败落 audit_state —— 重试凭它证明留痕。
	auditErr := s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherCreate,
		Summary: "录入凭证草稿（MCP）",
		Entity:  "voucher", EntityID: fmt.Sprintf("%d", vid),
		Detail: map[string]any{
			"记账日期": v.BizDate.String(), "摘要": v.Remark,
			"借方合计": money.Money(v.TotalDebit()).String(),
			"贷方合计": money.Money(v.TotalCredit()).String(),
			"分录":   auditLines(v), "制单人": v.CreatedBy,
			"clientRequestId": requestID,
		},
	})
	out := &IdemOutcome{}
	if auditErr != nil {
		s.db.MCPIdem().SetAuditState(ctx, requestID, "failed", auditErr.Error())
		out.AuditState, out.AuditError = "failed", auditErr.Error()
	} else {
		s.db.MCPIdem().SetAuditState(ctx, requestID, "recorded", "")
		out.AuditState = "recorded"
	}
	d, err := s.Voucher(ctx, vid)
	if err != nil {
		return nil, nil, err
	}
	return d, out, nil
}

// resolveIdem 命中已有幂等键时的回答（同内容才许命中）。
func (s *Service) resolveIdem(ctx context.Context, row *sqlite.IdemRow,
	requestHash string) (*VoucherDetail, *IdemOutcome, error) {
	if row.RequestHash != requestHash {
		return nil, nil, ErrIdempotencyConflict
	}
	d, err := s.Voucher(ctx, row.VoucherID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			return nil, nil, ErrIdempotentTargetDeleted
		}
		return nil, nil, err
	}
	out := &IdemOutcome{
		Idempotent:    true,
		ModifiedSince: d.UpdatedAt != row.InitialUpdatedAt,
	}
	switch row.AuditState {
	case "recorded":
		out.AuditState = "recorded"
	case "failed":
		// failed 表示当时确实没落日志 —— 补写是安全的（审计
		// 内容由已落库凭证确定性重建）。短暂锁竞争不该留
		// 永久缺口：同键重试到这里时顺手补记一条。
		if repairErr := s.repairCreateAudit(ctx, d, row.RequestID); repairErr != nil {
			s.db.MCPIdem().SetAuditState(ctx, row.RequestID, "failed", repairErr.Error())
			out.AuditState, out.AuditError = "failed", repairErr.Error()
		} else {
			s.db.MCPIdem().SetAuditState(ctx, row.RequestID, "recorded", "")
			out.AuditState = "recorded"
		}
	default:
		// pending：提交后崩溃，未确认已留痕 —— 如实告诉调用方。
		// 不自动补写：审计可能已经成功落库，再写一条就是
		// 无法分辨的重复记录，比缺口更难解释。
		out.AuditState, out.AuditError = "pending", "上次写入后未确认审计留痕"
	}
	return d, out, nil
}

// repairCreateAudit 为「审计写失败」的幂等命中补记创建日志。
// 事件内容与首次创建审计一致，数据全部取自已落库凭证；
// 「补记」标记让日志能看出这条不是当时写的。
func (s *Service) repairCreateAudit(ctx context.Context, d *VoucherDetail, requestID string) error {
	lines := make([]map[string]any, 0, len(d.Lines))
	for _, e := range d.Lines {
		lines = append(lines, map[string]any{
			"科目": e.AccountCode, "摘要": e.Summary,
			"借方": e.Debit.String(), "贷方": e.Credit.String(),
		})
	}
	return s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherCreate,
		Summary: "录入凭证草稿（MCP，幂等重试补记）",
		Entity:  "voucher", EntityID: fmt.Sprintf("%d", d.ID),
		Detail: map[string]any{
			"记账日期": d.Date, "摘要": d.Remark,
			"借方合计": d.TotalDebit.String(),
			"贷方合计": d.TotalCredit.String(),
			"分录":   lines, "制单人": d.CreatedBy,
			"clientRequestId": requestID,
		},
	})
}
