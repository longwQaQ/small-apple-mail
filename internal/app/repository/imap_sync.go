package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"small-apple-mail/internal/app/domain"
)

type IMAPSyncState struct{ UIDValidity, LastUID, AccountSignature string }

func (s *Store) IMAPSyncState(ctx context.Context, accountID string) (IMAPSyncState, error) {
	var state IMAPSyncState
	err := s.db.QueryRowContext(ctx, "SELECT uid_validity,last_uid,account_signature FROM imap_sync_states WHERE account_id=?", accountID).Scan(&state.UIDValidity, &state.LastUID, &state.AccountSignature)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return state, err
}

// SaveIMAPBatch 在同一事务中保存邮件和进度；旧凭据的在途同步不能覆盖新连接。
func (s *Store) SaveIMAPBatch(ctx context.Context, accountID, secret string, state IMAPSyncState, items []MessageInput) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var currentSecret string
	if err = tx.QueryRowContext(ctx, "SELECT secret_json FROM account_connections WHERE account_id=? AND kind='imap'", accountID).Scan(&currentSecret); err != nil {
		return 0, err
	}
	if currentSecret != secret {
		return 0, fmt.Errorf("收件连接已更新，本次同步结果未提交")
	}
	var previous IMAPSyncState
	err = tx.QueryRowContext(ctx, "SELECT uid_validity,last_uid,account_signature FROM imap_sync_states WHERE account_id=?", accountID).Scan(&previous.UIDValidity, &previous.LastUID, &previous.AccountSignature)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if previous.UIDValidity == "" || previous.UIDValidity != state.UIDValidity || previous.AccountSignature != state.AccountSignature {
		// 旧版记录的代次未知，也不能复用其 UID。历史邮件保留，但不再用于操作远端邮件。
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET remote_id='archived:' || ? || ':' || remote_id WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE account_id=?) AND remote_id LIKE 'imap:%'`, nowText(), accountID); err != nil {
			return 0, err
		}
	}
	n, err := saveMessagesTx(ctx, tx, items)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO imap_sync_states(account_id,uid_validity,last_uid,account_signature,last_synced_at) VALUES(?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET uid_validity=excluded.uid_validity,last_uid=excluded.last_uid,account_signature=excluded.account_signature,last_synced_at=excluded.last_synced_at`, accountID, state.UIDValidity, state.LastUID, state.AccountSignature, nowText())
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// 运行中收到的新通知合并为一个后续任务，避免去重丢掉运行期间到达的邮件。
func (s *Store) RequestMessageSyncJob(ctx context.Context, accountID string) (domain.Job, bool, error) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type='mailbox.message_sync' AND status='queued' AND json_valid(payload_json) AND json_extract(payload_json,'$.account_id')=? ORDER BY created_at LIMIT 1`, accountID).Scan(&id)
	if err == nil {
		job, err := s.Job(ctx, id)
		return job, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, err
	}
	payload, _ := json.Marshal(map[string]string{"account_id": accountID})
	job, err := s.CreateJob(ctx, "mailbox.message_sync", string(payload), 20)
	return job, err == nil, err
}
