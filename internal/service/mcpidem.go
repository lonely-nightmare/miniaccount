// create_draft_voucher 的幂等语义（文档 §2.5）。
//
// 键与凭证同一账套事务落库；提交后按审计成败写 audit_state ——
// 重试凭持久化状态回答「原操作到底留没留痕」。
package service

import (
	"context"
	"encoding/json"
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

	// 审计事件在落库前构造：明细原文随幂等键同事务存进
	// audit_payload。之后凭证被改过，failed 态的补记也照这份
	// 原文回放——「当前内容」会把修改记成初始创建，失真。
	auditSummary := "录入凭证草稿（MCP）"
	auditDetail := map[string]any{
		"记账日期": v.BizDate.String(), "摘要": v.Remark,
		"借方合计": money.Money(v.TotalDebit()).String(),
		"贷方合计": money.Money(v.TotalCredit()).String(),
		"分录":   auditLines(v), "制单人": v.CreatedBy,
		"clientRequestId": requestID,
	}
	payload, _ := json.Marshal(map[string]any{
		"summary": auditSummary, "detail": auditDetail,
	})

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
			AuditState: "pending", AuditPayload: string(payload),
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
		Summary: auditSummary,
		Entity:  "voucher", EntityID: fmt.Sprintf("%d", vid),
		Detail: auditDetail,
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
	case "failed", "repairing":
		// failed = 当时确实没落日志；repairing = 上次补记途中
		// 崩溃。两者都可补写 —— 但先 CAS 认领：并发重试只能
		// 有一个赢家写日志，否则一条创建日志会变成两条。
		claimed, cerr := s.db.MCPIdem().ClaimAuditRepair(ctx, row.RequestID)
		if cerr != nil {
			return nil, nil, cerr
		}
		if !claimed {
			// 别人在补：重读最新状态如实回答。
			if cur, gerr := s.db.MCPIdem().Get(ctx, row.RequestID); gerr == nil && cur != nil {
				out.AuditState = cur.AuditState
				out.AuditError = cur.AuditError
			} else {
				out.AuditState = "repairing"
			}
			return d, out, nil
		}
		// ★ 先查重再补写：上次「日志写成、SetAuditState 失败」
		// 会留下已落库的创建日志 + repairing 状态——不查重就
		// 再写，一条创建变两条（实测复现）。
		if s.hasCreateAudit(ctx, row) {
			s.db.MCPIdem().SetAuditState(ctx, row.RequestID, "recorded", "")
			out.AuditState = "recorded"
			return d, out, nil
		}
		if repairErr := s.repairCreateAudit(ctx, d, row); repairErr != nil {
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

// hasCreateAudit 查这张凭证是否已有带本 requestID 的创建日志
// —— 「Append 成功、SetAuditState 失败」留下的半完成态靠它
// 识别，避免重试再写一条。
// requestID 在 detail_json 里，Text 过滤缩小范围后再精确比对。
func (s *Service) hasCreateAudit(ctx context.Context, row *sqlite.IdemRow) bool {
	page, err := s.QueryAudit(ctx, AuditQuery{
		Actions: []string{string(audit.ActionVoucherCreate)},
		Text:    row.RequestID, Limit: 20,
	})
	if err != nil || page == nil {
		return false
	}
	vid := fmt.Sprintf("%d", row.VoucherID)
	for _, e := range page.Entries {
		if e.Entity == "voucher" && e.EntityID == vid && e.Book == s.path {
			if rid, ok := e.Detail["clientRequestId"].(string); ok && rid == row.RequestID {
				return true
			}
		}
	}
	return false
}

// repairCreateAudit 为「审计写失败」的幂等命中补记创建日志。
// 内容回放幂等行里存的 audit_payload（首次落库时的原文）：
// 凭证落库后被改过的话，拿「当前凭证」重建会把修改记成
// 初始创建内容 —— 日志失真，等于造假。payload 为空只可能
// 是旧版本写的幂等行，那时才退回按当前凭证重建并额外标注。
func (s *Service) repairCreateAudit(ctx context.Context, d *VoucherDetail, row *sqlite.IdemRow) error {
	var stored struct {
		Summary string         `json:"summary"`
		Detail  map[string]any `json:"detail"`
	}
	if row.AuditPayload != "" &&
		json.Unmarshal([]byte(row.AuditPayload), &stored) == nil &&
		stored.Detail != nil {
		return s.recordAudit(ctx, AuditEvent{
			Action:  audit.ActionVoucherCreate,
			Summary: stored.Summary + "（幂等重试补记）",
			Entity:  "voucher", EntityID: fmt.Sprintf("%d", d.ID),
			Detail: stored.Detail,
		})
	}
	lines := make([]map[string]any, 0, len(d.Lines))
	for _, e := range d.Lines {
		lines = append(lines, map[string]any{
			"科目": e.AccountCode, "摘要": e.Summary,
			"借方": e.Debit.String(), "贷方": e.Credit.String(),
		})
	}
	return s.recordAudit(ctx, AuditEvent{
		Action:  audit.ActionVoucherCreate,
		Summary: "录入凭证草稿（MCP，幂等重试补记，内容为当前状态）",
		Entity:  "voucher", EntityID: fmt.Sprintf("%d", d.ID),
		Detail: map[string]any{
			"记账日期": d.Date, "摘要": d.Remark,
			"借方合计": d.TotalDebit.String(),
			"贷方合计": d.TotalCredit.String(),
			"分录":   lines, "制单人": d.CreatedBy,
			"clientRequestId": row.RequestID,
		},
	})
}
