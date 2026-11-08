package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

func (s *Store) Account(ctx context.Context, id string) (domain.Account, error) {
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	for _, account := range rows {
		if account.ID == id {
			return account, nil
		}
	}
	return domain.Account{}, ErrNotFound
}

func (s *Store) DeleteConnection(ctx context.Context, accountID, kind string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM account_connections WHERE account_id=? AND kind=?", accountID, kind)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return s.SetAccountComputedStatus(ctx, accountID)
}

func (s *Store) SetAccountComputedStatus(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE apple_accounts SET status=(`+accountConnectionStatusSQL+`),updated_at=? WHERE id=?`, nowText(), accountID)
	return err
}

func (s *Store) UpdateAccount(ctx context.Context, id, name, note string) (domain.Account, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE apple_accounts SET display_name=?,note=?,updated_at=? WHERE id=?`, name, note, nowText(), id)
	if err != nil {
		return domain.Account{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Account{}, ErrNotFound
	}
	rows, err := s.ListAccounts(ctx)
	if err != nil {
		return domain.Account{}, err
	}
	for _, a := range rows {
		if a.ID == id {
			return a, nil
		}
	}
	return domain.Account{}, ErrNotFound
}
func (s *Store) AccountImpact(ctx context.Context, id string) (map[string]int, error) {
	out := map[string]int{}
	queries := map[string]string{"connections": "SELECT COUNT(*) FROM account_connections WHERE account_id=?", "mailboxes": "SELECT COUNT(*) FROM mailboxes WHERE account_id=?", "active_leases": "SELECT COUNT(*) FROM leases l JOIN mailboxes m ON m.id=l.mailbox_id WHERE m.account_id=? AND l.status='active'", "messages": "SELECT COUNT(*) FROM messages msg JOIN mailboxes m ON m.id=msg.mailbox_id WHERE m.account_id=?"}
	for key, q := range queries {
		var n int
		if err := s.db.QueryRowContext(ctx, q, id).Scan(&n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	return out, nil
}
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	impact, err := s.AccountImpact(ctx, id)
	if err != nil {
		return err
	}
	if impact["active_leases"] > 0 {
		return fmt.Errorf("账号仍有活动租约")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE account_id=?)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM leases WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE account_id=?)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_api_credentials WHERE mailbox_id IN (SELECT id FROM mailboxes WHERE account_id=?)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM mailboxes WHERE account_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM apple_accounts WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) UpdateMailbox(ctx context.Context, id string, label, note, status *string, localEnabled *bool, tagsJSON *string) (domain.Mailbox, error) {
	sets := []string{"updated_at=?"}
	args := []any{nowText()}
	if label != nil {
		sets = append(sets, "label=?")
		args = append(args, strings.TrimSpace(*label))
	}
	if note != nil {
		sets = append(sets, "note=?")
		args = append(args, strings.TrimSpace(*note))
	}
	if status != nil {
		sets = append(sets, "status=?")
		args = append(args, strings.TrimSpace(*status))
	}
	if localEnabled != nil {
		sets = append(sets, "local_enabled=?", "status=CASE WHEN status='leased' THEN status WHEN ?=0 THEN 'disabled' WHEN remote_present=0 THEN 'remote_missing' WHEN remote_enabled=0 THEN 'disabled' ELSE 'available' END")
		args = append(args, *localEnabled, *localEnabled)
	}
	if tagsJSON != nil {
		sets = append(sets, "tags_json=?")
		args = append(args, *tagsJSON)
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET `+joinComma(sets)+` WHERE id=?`, args...)
	if err != nil {
		return domain.Mailbox{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Mailbox{}, ErrNotFound
	}
	return s.Mailbox(ctx, id)
}
func joinComma(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
func (s *Store) DeleteMailbox(ctx context.Context, id string) error {
	var active int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE mailbox_id=? AND status='active'", id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("邮箱仍有活动租约")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM messages WHERE mailbox_id=?", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM leases WHERE mailbox_id=?", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM mailbox_api_credentials WHERE mailbox_id=?", id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM mailboxes WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) Mailbox(ctx context.Context, id string) (domain.Mailbox, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Mailbox{}, err
	}
	defer tx.Rollback()
	mailbox, err := mailboxByIDTx(ctx, tx, id)
	if err != nil {
		return mailbox, err
	}
	return mailbox, tx.Commit()
}

func (s *Store) ListMessages(ctx context.Context, mailboxID string, limit int) ([]domain.Message, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,mailbox_id,remote_id,sender,subject,code,login_url,match_rule,confidence,received_at,synced_at,consumed_at,COALESCE(consumed_lease_id,'') FROM messages WHERE mailbox_id=? ORDER BY `+messageTimeSQL+` DESC,id DESC LIMIT ?`, mailboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		var received, synced, consumed sql.NullString
		if err := rows.Scan(&m.ID, &m.MailboxID, &m.RemoteID, &m.Sender, &m.Subject, &m.Code, &m.LoginURL, &m.MatchRule, &m.Confidence, &received, &synced, &consumed, &m.ConsumedLeaseID); err != nil {
			return nil, err
		}
		m.ReceivedAt, m.SyncedAt, m.ConsumedAt = parseTime(received), parseTime(synced), parseTime(consumed)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListMessagesPage(ctx context.Context, mailboxID string, page, pageSize int) ([]domain.Message, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 10
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE mailbox_id=?`, mailboxID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,mailbox_id,remote_id,sender,subject,code,login_url,match_rule,confidence,received_at,synced_at,consumed_at,COALESCE(consumed_lease_id,'') FROM messages WHERE mailbox_id=? ORDER BY `+messageTimeSQL+` DESC,id DESC LIMIT ? OFFSET ?`, mailboxID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]domain.Message, 0, pageSize)
	for rows.Next() {
		var m domain.Message
		var received, synced, consumed sql.NullString
		if err := rows.Scan(&m.ID, &m.MailboxID, &m.RemoteID, &m.Sender, &m.Subject, &m.Code, &m.LoginURL, &m.MatchRule, &m.Confidence, &received, &synced, &consumed, &m.ConsumedLeaseID); err != nil {
			return nil, 0, err
		}
		m.ReceivedAt, m.SyncedAt, m.ConsumedAt = parseTime(received), parseTime(synced), parseTime(consumed)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) Message(ctx context.Context, mailboxID, messageID string) (domain.Message, error) {
	var m domain.Message
	var received, synced, consumed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,mailbox_id,remote_id,sender,subject,code,login_url,match_rule,confidence,body,received_at,synced_at,consumed_at,COALESCE(consumed_lease_id,'') FROM messages WHERE mailbox_id=? AND id=?`, mailboxID, messageID).Scan(&m.ID, &m.MailboxID, &m.RemoteID, &m.Sender, &m.Subject, &m.Code, &m.LoginURL, &m.MatchRule, &m.Confidence, &m.Body, &received, &synced, &consumed, &m.ConsumedLeaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.ReceivedAt, m.SyncedAt, m.ConsumedAt = parseTime(received), parseTime(synced), parseTime(consumed)
	return m, nil
}

func (s *Store) Job(ctx context.Context, id string) (domain.Job, error) {
	var j domain.Job
	var created, started, finished sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,type,status,priority,payload_json,progress,attempts,error_code,error_message,created_at,started_at,finished_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.Type, &j.Status, &j.Priority, &j.Payload, &j.Progress, &j.Attempts, &j.ErrorCode, &j.Error, &created, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	j.CreatedAt, j.StartedAt, j.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
	return j, err
}

func (s *Store) RotateAPIClient(ctx context.Context, id string) (string, error) {
	key, err := security.RandomToken("sam_live_", 32)
	if err != nil {
		return "", err
	}
	prefix := key
	if len(prefix) > 18 {
		prefix = prefix[:18]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE api_clients SET key_prefix=?,key_hash=?,updated_at=? WHERE id=?`, prefix, security.HashToken(key), nowText(), id)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", ErrNotFound
	}
	return key, nil
}
func (s *Store) SetAPIClientStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("无效状态")
	}
	res, err := s.db.ExecContext(ctx, "UPDATE api_clients SET status=?,updated_at=? WHERE id=?", status, nowText(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) DeleteAPIClient(ctx context.Context, id string) error {
	var active int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE api_client_id=? AND status='active'", id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("客户端仍有活动租约")
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM api_clients WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

type scheduleScanner interface {
	Scan(dest ...any) error
}

func scanSchedule(scanner scheduleScanner) (domain.Schedule, error) {
	var v domain.Schedule
	var enabled int
	var next, last, created, updated sql.NullString
	if err := scanner.Scan(&v.ID, &v.Type, &v.Name, &enabled, &v.Payload, &v.IntervalSeconds, &v.ConsecutiveFailures, &v.PausedReason, &next, &last, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Schedule{}, ErrNotFound
		}
		return domain.Schedule{}, err
	}
	v.Enabled = enabled != 0
	v.NextRunAt, v.LastRunAt, v.CreatedAt, v.UpdatedAt = parseTime(next), parseTime(last), parseTime(created), parseTime(updated)
	return v, nil
}

func validateSchedule(name, payload string, interval int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("计划名称不能为空")
	}
	if len([]rune(strings.TrimSpace(name))) > 80 {
		return fmt.Errorf("计划名称最多 80 个字符")
	}
	if interval < 60 || interval > 365*24*60*60 {
		return fmt.Errorf("计划间隔必须在 60 秒到 365 天之间")
	}
	var parameters map[string]any
	if err := json.Unmarshal([]byte(payload), &parameters); err != nil || parameters == nil {
		return fmt.Errorf("计划参数必须是 JSON 对象")
	}
	return nil
}

func (s *Store) ListSchedules(ctx context.Context) ([]domain.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,name,enabled,payload_json,interval_seconds,consecutive_failures,paused_reason,next_run_at,last_run_at,created_at,updated_at FROM schedules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Schedule
	for rows.Next() {
		v, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Schedule(ctx context.Context, id string) (domain.Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, `SELECT id,type,name,enabled,payload_json,interval_seconds,consecutive_failures,paused_reason,next_run_at,last_run_at,created_at,updated_at FROM schedules WHERE id=?`, id))
}

func (s *Store) CreateSchedule(ctx context.Context, typ, name, payload string, enabled bool, interval int) (domain.Schedule, error) {
	name = strings.TrimSpace(name)
	payload = strings.TrimSpace(payload)
	if payload == "" {
		payload = "{}"
	}
	if err := validateSchedule(name, payload, interval); err != nil {
		return domain.Schedule{}, err
	}
	id, _ := security.ID("sch")
	now := time.Now().UTC()
	v := domain.Schedule{ID: id, Type: typ, Name: name, Enabled: enabled, Payload: payload, IntervalSeconds: interval, NextRunAt: now.Add(time.Duration(interval) * time.Second), CreatedAt: now, UpdatedAt: now}
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules(id,type,name,enabled,payload_json,interval_seconds,next_run_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, typ, name, enabled, payload, interval, timeText(v.NextRunAt), timeText(now), timeText(now))
	return v, err
}

func (s *Store) UpdateSchedule(ctx context.Context, id, typ, name, payload string, enabled bool, interval int) (domain.Schedule, error) {
	name = strings.TrimSpace(name)
	payload = strings.TrimSpace(payload)
	if err := validateSchedule(name, payload, interval); err != nil {
		return domain.Schedule{}, err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `UPDATE schedules SET type=?,name=?,payload_json=?,enabled=?,interval_seconds=?,consecutive_failures=CASE WHEN ?=1 AND (enabled=0 OR paused_reason<>'') THEN 0 ELSE consecutive_failures END,paused_reason=CASE WHEN ?=1 THEN '' ELSE paused_reason END,next_run_at=?,updated_at=? WHERE id=?`, typ, name, payload, enabled, interval, enabled, enabled, timeText(now.Add(time.Duration(interval)*time.Second)), timeText(now), id)
	if err != nil {
		return domain.Schedule{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Schedule{}, ErrNotFound
	}
	return s.Schedule(ctx, id)
}
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) RunScheduleNow(ctx context.Context, id string) (domain.Job, error) {
	var typ, payload string
	if err := s.db.QueryRowContext(ctx, "SELECT type,payload_json FROM schedules WHERE id=?", id).Scan(&typ, &payload); errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	} else if err != nil {
		return domain.Job{}, err
	}
	return s.CreateJob(ctx, typ, payload, 5)
}
func (s *Store) EnqueueDueSchedules(ctx context.Context) ([]domain.Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.type,s.payload_json,s.interval_seconds FROM schedules s WHERE s.enabled=1 AND s.paused_reason='' AND s.next_run_at<=? AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.schedule_id=s.id AND j.status IN ('queued','running','cancelling')) ORDER BY s.next_run_at LIMIT 20`, nowText())
	if err != nil {
		return nil, err
	}
	type due struct {
		id, typ, payload string
		interval         int
	}
	var items []due
	for rows.Next() {
		var v due
		if err := rows.Scan(&v.id, &v.typ, &v.payload, &v.interval); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, v)
	}
	rows.Close()
	var jobs []domain.Job
	now := time.Now().UTC()
	for _, item := range items {
		if item.typ == "account.connection.check" {
			var payload map[string]any
			if err := json.Unmarshal([]byte(item.payload), &payload); err != nil {
				return nil, fmt.Errorf("定时连接检测参数无效")
			}
			if payload == nil {
				payload = make(map[string]any)
			}
			payload["automatic"] = true
			encoded, _ := json.Marshal(payload)
			item.payload = string(encoded)
		}
		id, _ := security.ID("job")
		job := domain.Job{ID: id, Type: item.typ, Status: domain.JobQueued, Priority: 0, Payload: item.payload, CreatedAt: now}
		if _, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,type,status,priority,payload_json,schedule_id,created_at) VALUES(?,?,?,?,?,?,?)`, job.ID, job.Type, job.Status, 0, job.Payload, item.id, timeText(now)); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE schedules SET last_run_at=?,next_run_at=?,updated_at=? WHERE id=?`, timeText(now), timeText(now.Add(time.Duration(item.interval)*time.Second)), timeText(now), item.id); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, tx.Commit()
}

func (s *Store) JobEvents(ctx context.Context, id string) ([]domain.JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,job_id,stage,level,message,detail_json,created_at FROM job_events WHERE job_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.JobEvent
	for rows.Next() {
		var e domain.JobEvent
		var created sql.NullString
		if err := rows.Scan(&e.ID, &e.JobID, &e.Stage, &e.Level, &e.Message, &e.Detail, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}
