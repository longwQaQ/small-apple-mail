package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

func (s *Store) ListConnections(ctx context.Context, accountID string) ([]domain.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,account_id,kind,status,host,last_checked_at,expires_at,next_check_at,consecutive_failures,status_message,created_at,updated_at FROM account_connections WHERE account_id=? ORDER BY kind`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Connection
	for rows.Next() {
		var c domain.Connection
		var checked, expires, nextCheck, created, updated sql.NullString
		if err := rows.Scan(&c.ID, &c.AccountID, &c.Kind, &c.Status, &c.Host, &checked, &expires, &nextCheck, &c.ConsecutiveFailures, &c.StatusMessage, &created, &updated); err != nil {
			return nil, err
		}
		c.LastCheckedAt, c.ExpiresAt, c.NextCheckAt, c.CreatedAt, c.UpdatedAt = parseTime(checked), parseTime(expires), parseTime(nextCheck), parseTime(created), parseTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpsertConnection(ctx context.Context, accountID, kind, status, host, secretJSON, message string) (domain.Connection, error) {
	if !domain.ValidConnectionKinds[kind] {
		return domain.Connection{}, fmt.Errorf("无效连接类型")
	}
	id, _ := security.ID("con")
	now := nowText()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_connections(id,account_id,kind,status,host,secret_json,status_message,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id,kind) DO UPDATE SET status=excluded.status,host=excluded.host,secret_json=excluded.secret_json,status_message=excluded.status_message,last_checked_at=NULL,expires_at=NULL,next_check_at=NULL,consecutive_failures=0,updated_at=excluded.updated_at`, id, accountID, kind, status, host, secretJSON, message, now, now)
	if err != nil {
		return domain.Connection{}, err
	}
	var out domain.Connection
	var checked, expires, nextCheck, created, updated sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT id,account_id,kind,status,host,secret_json,last_checked_at,expires_at,next_check_at,consecutive_failures,status_message,created_at,updated_at FROM account_connections WHERE account_id=? AND kind=?`, accountID, kind).Scan(&out.ID, &out.AccountID, &out.Kind, &out.Status, &out.Host, &out.SecretJSON, &checked, &expires, &nextCheck, &out.ConsecutiveFailures, &out.StatusMessage, &created, &updated)
	out.LastCheckedAt, out.ExpiresAt, out.NextCheckAt, out.CreatedAt, out.UpdatedAt = parseTime(checked), parseTime(expires), parseTime(nextCheck), parseTime(created), parseTime(updated)
	return out, err
}

func (s *Store) ConnectionSecret(ctx context.Context, accountID, kind string) (domain.Connection, error) {
	var out domain.Connection
	var checked, expires, nextCheck, created, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,account_id,kind,status,host,secret_json,last_checked_at,expires_at,next_check_at,consecutive_failures,status_message,created_at,updated_at FROM account_connections WHERE account_id=? AND kind=?`, accountID, kind).Scan(&out.ID, &out.AccountID, &out.Kind, &out.Status, &out.Host, &out.SecretJSON, &checked, &expires, &nextCheck, &out.ConsecutiveFailures, &out.StatusMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	out.LastCheckedAt, out.ExpiresAt, out.NextCheckAt, out.CreatedAt, out.UpdatedAt = parseTime(checked), parseTime(expires), parseTime(nextCheck), parseTime(created), parseTime(updated)
	return out, err
}

func (s *Store) SetConnectionStatus(ctx context.Context, accountID, kind, status, message string) error {
	_, err := s.setConnectionStatus(ctx, accountID, kind, status, message, nil)
	return err
}

// 旧监听器退出前的回调不能覆盖已更新的 IMAP 凭据状态。
func (s *Store) SetIMAPWatchStatus(ctx context.Context, accountID, secret, status, message string) (bool, error) {
	return s.setConnectionStatus(ctx, accountID, domain.ConnectionIMAP, status, message, &secret)
}

func (s *Store) setConnectionStatus(ctx context.Context, accountID, kind, status, message string, expectedSecret *string) (bool, error) {
	now := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := `UPDATE account_connections SET status=?,status_message=?,last_checked_at=?,updated_at=? WHERE account_id=? AND kind=?`
	args := []any{status, message, now, now, accountID, kind}
	if expectedSecret != nil {
		query += ` AND secret_json=? AND status<>'disabled'`
		args = append(args, *expectedSecret)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	if err := updateAccountConnectionStatus(ctx, tx, accountID, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// 账号总状态由基础连接决定，备用创建通道的状态单独展示。
const accountConnectionStatusSQL = `CASE
 WHEN EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=apple_accounts.id AND c.kind IN ('icloud_web','imap') AND c.status IN ('failed','expired','rate_limited')) THEN 'failed'
 WHEN EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=apple_accounts.id AND c.kind IN ('icloud_web','imap') AND c.status='pending_2fa') THEN 'pending_2fa'
 WHEN EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=apple_accounts.id AND c.kind IN ('icloud_web','imap') AND c.status='checking') THEN 'checking'
 WHEN EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=apple_accounts.id AND c.kind IN ('icloud_web','imap') AND c.status IN ('healthy','expiring')) THEN 'healthy'
 ELSE 'not_configured' END`

func updateAccountConnectionStatus(ctx context.Context, tx *sql.Tx, accountID, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE apple_accounts SET status=(`+accountConnectionStatusSQL+`),updated_at=? WHERE id=?`, now, accountID)
	return err
}

// 仅切换已保存的备用通道；停用保留凭据，版本变化阻止在途旧任务重新启用它。
func (s *Store) SetOptionalConnectionEnabled(ctx context.Context, accountID string, enabled bool) (domain.Connection, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Connection{}, err
	}
	defer tx.Rollback()
	status, message, condition := domain.ConnectionDisabled, "备用创建通道已停用，登录凭据已保留", "status<>'disabled'"
	if enabled {
		status, message, condition = domain.ConnectionChecking, "备用创建通道已启用，等待检测", "status='disabled'"
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_connections WHERE account_id=? AND kind='apple_account'`, accountID).Scan(&exists); err != nil {
		return domain.Connection{}, err
	}
	if exists == 0 {
		return domain.Connection{}, ErrNotFound
	}
	now := nowText()
	if _, err := tx.ExecContext(ctx, `UPDATE account_connections SET status=?,status_message=?,next_check_at=NULL,consecutive_failures=0,updated_at=? WHERE account_id=? AND kind='apple_account' AND `+condition, status, message, now, accountID); err != nil {
		return domain.Connection{}, err
	}
	if err := updateAccountConnectionStatus(ctx, tx, accountID, now); err != nil {
		return domain.Connection{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Connection{}, err
	}
	return s.ConnectionSecret(ctx, accountID, domain.ConnectionAppleAccount)
}

// SaveConnectionCheck 原子保存会话和维护结果。登录更新或删除后，旧任务不得覆盖新状态。
func (s *Store) SaveConnectionCheck(ctx context.Context, before, after domain.Connection) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := nowText()
	res, err := tx.ExecContext(ctx, `UPDATE account_connections SET secret_json=?,host=?,status=?,status_message=?,last_checked_at=?,expires_at=?,next_check_at=?,consecutive_failures=?,updated_at=? WHERE id=? AND updated_at=?`, after.SecretJSON, after.Host, after.Status, after.StatusMessage, now, timeText(after.ExpiresAt), timeText(after.NextCheckAt), after.ConsecutiveFailures, now, before.ID, timeText(before.UpdatedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err = updateAccountConnectionStatus(ctx, tx, before.AccountID, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) EnsureConnectionCheckJob(ctx context.Context, accountID, kind string, automatic bool) (domain.Job, bool, error) {
	if accountID == "" || !domain.ValidConnectionKinds[kind] {
		return domain.Job{}, false, fmt.Errorf("无效连接检测目标")
	}
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type='account.connection.check' AND status IN ('queued','running','cancelling') AND json_valid(payload_json) AND json_extract(payload_json,'$.account_id')=? AND json_extract(payload_json,'$.kind')=? ORDER BY created_at LIMIT 1`, accountID, kind).Scan(&id)
	if err == nil {
		job, e := s.Job(ctx, id)
		return job, false, e
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, err
	}
	payload, _ := json.Marshal(map[string]any{"account_id": accountID, "kind": kind, "automatic": automatic})
	priority := 10
	if automatic {
		priority = 0
	}
	job, err := s.CreateJob(ctx, "account.connection.check", string(payload), priority)
	return job, err == nil, err
}

func (s *Store) EnqueueConnectionMaintenance(ctx context.Context) ([]domain.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.account_id,c.kind FROM account_connections c WHERE c.kind IN ('apple_account','icloud_web') AND c.status IN ('healthy','expiring','failed','rate_limited','checking') AND (c.next_check_at IS NULL OR c.next_check_at<=?) AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.type='account.connection.check' AND j.status IN ('queued','running','cancelling') AND json_valid(j.payload_json) AND json_extract(j.payload_json,'$.account_id')=c.account_id AND json_extract(j.payload_json,'$.kind')=c.kind) ORDER BY COALESCE(c.next_check_at,c.updated_at) LIMIT 20`, nowText())
	if err != nil {
		return nil, err
	}
	var targets [][2]string
	for rows.Next() {
		var target [2]string
		if err = rows.Scan(&target[0], &target[1]); err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var jobs []domain.Job
	for _, target := range targets {
		job, created, e := s.EnsureConnectionCheckJob(ctx, target[0], target[1], true)
		if e != nil {
			return jobs, e
		}
		if created {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
