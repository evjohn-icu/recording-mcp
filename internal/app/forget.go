package app

import (
	"context"
	"os"
	"recordingmcp/internal/memory"
	"strings"
)

type ForgetPreview struct {
	memory.ForgetPreview
	ManagedBackupCount   int    `json:"managed_backup_count"`
	IndexRebuildRequired bool   `json:"index_rebuild_required"`
	RetentionNotice      string `json:"retention_notice"`
}

func (s *Service) ManagedRetentionNotice() (int, string, error) {
	count := 0
	if s.ManagedBackupDir != "" {
		entries, err := os.ReadDir(s.ManagedBackupDir)
		if err != nil && !os.IsNotExist(err) {
			return 0, "", err
		}
		count = len(entries)
	}
	return count, "为清除副本，将移除此资料库的全部受管理备份，并重建检索索引。云端、手工导出和系统备份不在此清理范围内。", nil
}

func (s *Service) PreviewForget(ctx context.Context, req memory.ForgetRequest) (ForgetPreview, error) {
	p, err := s.Store.PreviewForget(ctx, req)
	if err != nil {
		return ForgetPreview{}, forgetError(err)
	}
	n, notice, err := s.ManagedRetentionNotice()
	if err == nil {
		notice += "遗忘操作还会清理同会话中无法确认来源的旧事件副本；来源可独立确认的其他事实会保留。"
	}
	return ForgetPreview{ForgetPreview: p, ManagedBackupCount: n, IndexRebuildRequired: s.ManagedIndexDir != "", RetentionNotice: notice}, err
}

// ForgetOperation retains the durable operation ID even when external cleanup
// needs retry. A pending response never claims permanent erasure completed.
type ForgetOperation struct {
	memory.ForgetResult
	CleanupPending bool   `json:"cleanup_pending"`
	Message        string `json:"message,omitempty"`
}

func (s *Service) CommitForget(ctx context.Context, req memory.ForgetRequest) (ForgetOperation, error) {
	result, err := s.Store.CommitForget(ctx, req)
	if err != nil {
		return ForgetOperation{ForgetResult: result}, forgetError(err)
	}
	out := ForgetOperation{ForgetResult: result}
	if err := s.finishForget(ctx, result.TombstoneID); err != nil {
		out.CleanupPending = true
		out.Message = "清理尚未完成，请重试。删除屏障已生效。"
		return out, nil
	}
	out.Status = "complete"
	return out, nil
}
func (s *Service) finishForget(ctx context.Context, id string) error {
	if err := s.Store.ResumeForget(ctx, id); err != nil {
		return err
	}
	if err := s.CleanupForget(ctx, id); err != nil {
		return err
	}
	return s.Store.CompleteForget(ctx, id)
}
func (s *Service) RetryForget(ctx context.Context, id string) (memory.ForgetStatus, error) {
	if err := s.finishForget(ctx, id); err != nil {
		return memory.ForgetStatus{}, err
	}
	return s.Store.ForgetStatus(ctx, id)
}

// PendingForgetOperation is intentionally path-free: status discovery must
// not expose stored audio locations or invoke any cleanup work.
type PendingForgetOperation struct {
	ID             string `json:"id"`
	Scope          string `json:"scope"`
	ConversationID string `json:"conversation_id,omitempty"`
	RecordingID    string `json:"recording_id,omitempty"`
	RequestedID    string `json:"requested_id"`
	Status         string `json:"status"`
	RedactionDone  bool   `json:"redaction_done"`
}

func (s *Service) PendingForgets(ctx context.Context) ([]PendingForgetOperation, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT id FROM forget_tombstones WHERE status='pending_external_cleanup' ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]PendingForgetOperation, 0, len(ids))
	for _, id := range ids {
		status, err := s.Store.ForgetStatus(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, PendingForgetOperation{ID: status.ID, Scope: status.Scope, ConversationID: status.ConversationID, RecordingID: status.RecordingID, RequestedID: status.RequestedID, Status: status.Status, RedactionDone: status.RedactionDone})
	}
	return out, nil
}

func forgetError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "preview is stale") {
		return &Error{Code: "forget_version_conflict", Status: 409, Message: "记录已变化，请重新查看删除预览；尚未覆盖你的选择。"}
	}
	if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "audio offsets") || strings.Contains(err.Error(), "scope must") || strings.Contains(err.Error(), "timing") {
		return validationError(err.Error())
	}
	return err
}
func (s *Service) SuppressAINote(ctx context.Context, recordingID string, sourceSegmentID string, kind string) error {
	return s.Store.SuppressAINote(ctx, recordingID, sourceSegmentID, kind)
}

type SuppressionOperation struct {
	OperationID    string `json:"operation_id"`
	Status         string `json:"status"`
	CleanupPending bool   `json:"cleanup_pending"`
	Message        string `json:"message,omitempty"`
}

func (s *Service) SuppressNote(ctx context.Context, conversationID, noteID, expectedVersion string) (SuppressionOperation, error) {
	// An idempotent retry after suppression no longer needs the optimistic
	// version. The store looks up the durable operation before checking it.
	operationID, err := s.Store.SuppressNoteOperation(ctx, conversationID, noteID, expectedVersion)
	if err != nil {
		if strings.Contains(err.Error(), "version") {
			return SuppressionOperation{}, &Error{Code: CodeAnnotationVersionConflict, Status: 409, Message: "归纳版本已变化，请刷新后重新核对。"}
		}
		if strings.Contains(err.Error(), "required") {
			return SuppressionOperation{}, validationError(err.Error())
		}
		return SuppressionOperation{}, err
	}
	return s.retrySuppressionCleanup(ctx, operationID)
}

func (s *Service) SuppressionCleanupStatus(ctx context.Context, operationID string) (SuppressionOperation, error) {
	op, err := s.Store.SuppressionCleanupStatus(ctx, operationID)
	if err != nil {
		return SuppressionOperation{}, err
	}
	if op.Status == "pending" {
		return SuppressionOperation{OperationID: op.OperationID, Status: "pending_external_cleanup", CleanupPending: true}, nil
	}
	return SuppressionOperation{OperationID: op.OperationID, Status: "complete"}, nil
}

func (s *Service) PendingSuppressionCleanups(ctx context.Context) ([]SuppressionOperation, error) {
	ops, err := s.Store.PendingSuppressionCleanups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SuppressionOperation, 0, len(ops))
	for _, op := range ops {
		out = append(out, SuppressionOperation{OperationID: op.OperationID, Status: "pending_external_cleanup", CleanupPending: true})
	}
	return out, nil
}

func (s *Service) RetrySuppressionCleanup(ctx context.Context, operationID string) (SuppressionOperation, error) {
	return s.retrySuppressionCleanup(ctx, operationID)
}

func (s *Service) retrySuppressionCleanup(ctx context.Context, operationID string) (SuppressionOperation, error) {
	op, err := s.Store.SuppressionCleanupStatus(ctx, operationID)
	if err != nil {
		return SuppressionOperation{}, err
	}
	if op.Status == "complete" {
		return SuppressionOperation{OperationID: op.OperationID, Status: op.Status}, nil
	}
	if err := s.cleanupSuppressedNote(ctx); err != nil {
		return SuppressionOperation{OperationID: op.OperationID, Status: "pending_external_cleanup", CleanupPending: true, Message: "清理尚未完成，请重试。"}, nil
	}
	if err := s.Store.CompleteSuppressionCleanup(ctx, operationID); err != nil {
		return SuppressionOperation{OperationID: op.OperationID, Status: "pending_external_cleanup", CleanupPending: true, Message: "清理尚未完成，请重试。"}, nil
	}
	return SuppressionOperation{OperationID: op.OperationID, Status: "complete"}, nil
}

func (s *Service) cleanupSuppressedNote(ctx context.Context) error {
	// Plane events have turn/revision provenance, but no note provenance. A
	// conversation-wide purge could erase unrelated user-entered facts, so do
	// not remove event rows without an explicit reference to this note.
	if index, ok := s.Semantic.(interface{ Invalidate() }); ok {
		index.Invalidate()
	}
	for _, dir := range []string{s.ManagedIndexDir, s.ManagedBackupDir} {
		if dir != "" {
			if err := clearManagedDirectory(dir); err != nil {
				return err
			}
		}
	}
	return nil
}
