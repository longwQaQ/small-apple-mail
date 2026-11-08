package repository

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

type LeaseRequest struct {
	Project        string   `json:"project"`
	Purpose        string   `json:"purpose"`
	TTLSeconds     int      `json:"ttl_seconds"`
	Tags           []string `json:"tags"`
	IdempotencyKey string   `json:"-"`
}

func NormalizeLeaseTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return 30 * time.Minute
	}
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return time.Minute
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func normalizeTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] {
			continue
		}
		if len([]rune(tag)) > 32 {
			return nil, fmt.Errorf("单个标签不能超过 32 个字符")
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > 20 {
		return nil, fmt.Errorf("标签最多 20 个")
	}
	sort.Strings(out)
	return out, nil
}

func leaseFingerprint(req LeaseRequest, ttl time.Duration, tags []string) string {
	payload, _ := json.Marshal(map[string]any{"project": strings.TrimSpace(req.Project), "purpose": strings.TrimSpace(req.Purpose), "ttl_seconds": int(ttl / time.Second), "tags": tags})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func deterministicLeaseToken(clientID, key, requestHash string) string {
	mac := hmac.New(sha256.New, []byte(clientID))
	_, _ = mac.Write([]byte(key + ":" + requestHash))
	return "sam_lease_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Store) CreateLease(ctx context.Context, client domain.APIClient, req LeaseRequest) (domain.Lease, domain.Mailbox, string, bool, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 128 {
		return domain.Lease{}, domain.Mailbox{}, "", false, fmt.Errorf("Idempotency-Key 必填且不能超过 128 字符")
	}
	tags, err := normalizeTags(req.Tags)
	if err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	ttl := NormalizeLeaseTTL(req.TTLSeconds)
	fingerprint := leaseFingerprint(req, ttl, tags)
	token := deterministicLeaseToken(client.ID, req.IdempotencyKey, fingerprint)
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	defer tx.Rollback()
	var existing domain.Lease
	var created, updated, expires, released sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,mailbox_id,api_client_id,project,purpose,token_hash,idempotency_key,request_hash,status,created_at,updated_at,expires_at,released_at FROM leases WHERE api_client_id=? AND idempotency_key=? AND created_at>? ORDER BY created_at DESC LIMIT 1`, client.ID, req.IdempotencyKey, timeText(now.Add(-24*time.Hour))).Scan(&existing.ID, &existing.MailboxID, &existing.APIClientID, &existing.Project, &existing.Purpose, &existing.TokenHash, &existing.IdempotencyKey, &existing.RequestHash, &existing.Status, &created, &updated, &expires, &released)
	if err == nil {
		if existing.RequestHash != fingerprint {
			return domain.Lease{}, domain.Mailbox{}, "", false, ErrIdempotencyConflict
		}
		existing.CreatedAt, existing.UpdatedAt, existing.ExpiresAt, existing.ReleasedAt = parseTime(created), parseTime(updated), parseTime(expires), parseTime(released)
		mb, err := mailboxByIDTx(ctx, tx, existing.MailboxID)
		return existing, mb, token, true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	mailbox, err := selectMailboxForLease(ctx, tx, tags)
	if err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	id, _ := security.ID("lease")
	lease := domain.Lease{ID: id, MailboxID: mailbox.ID, APIClientID: client.ID, Project: strings.TrimSpace(req.Project), Purpose: strings.TrimSpace(req.Purpose), TokenHash: security.HashToken(token), IdempotencyKey: req.IdempotencyKey, RequestHash: fingerprint, Status: domain.LeaseActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err = tx.ExecContext(ctx, `INSERT INTO leases(id,mailbox_id,api_client_id,project,purpose,token_hash,idempotency_key,request_hash,status,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, lease.ID, lease.MailboxID, lease.APIClientID, lease.Project, lease.Purpose, lease.TokenHash, lease.IdempotencyKey, lease.RequestHash, lease.Status, timeText(now), timeText(now), timeText(lease.ExpiresAt))
	if err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET status='leased',last_leased_at=?,updated_at=? WHERE id=? AND status='available'`, timeText(now), timeText(now), mailbox.ID)
	if err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Lease{}, domain.Mailbox{}, "", false, err
	}
	mailbox.Status = domain.MailboxLeased
	mailbox.LastLeasedAt = now
	return lease, mailbox, token, false, nil
}

var ErrPoolEmpty = errors.New("mailbox pool empty")
var ErrIdempotencyConflict = errors.New("idempotency conflict")
var ErrLeaseInactive = errors.New("lease inactive")

func selectMailboxForLease(ctx context.Context, tx *sql.Tx, tags []string) (domain.Mailbox, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,email,COALESCE(remote_anonymous_id,''),label,note,allocation_mode,purpose,tags_json,status,remote_status,local_enabled,remote_present,remote_enabled,last_leased_at,last_synced_at,last_code_at,created_at,updated_at FROM mailboxes WHERE status='available' AND allocation_mode='shared' AND local_enabled=1 AND remote_present=1 AND remote_enabled=1 ORDER BY CASE WHEN last_leased_at IS NULL THEN 0 ELSE 1 END,last_leased_at,created_at`)
	if err != nil {
		return domain.Mailbox{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m domain.Mailbox
		var tagJSON string
		var local, present, enabled int
		var leased, synced, code, created, updated sql.NullString
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Email, &m.RemoteAnonymousID, &m.Label, &m.Note, &m.AllocationMode, &m.Purpose, &tagJSON, &m.Status, &m.RemoteStatus, &local, &present, &enabled, &leased, &synced, &code, &created, &updated); err != nil {
			return m, err
		}
		_ = json.Unmarshal([]byte(tagJSON), &m.Tags)
		if containsTags(m.Tags, tags) {
			m.LocalEnabled, m.RemotePresent, m.RemoteEnabled = local != 0, present != 0, enabled != 0
			m.LastLeasedAt, m.LastSyncedAt, m.LastCodeAt, m.CreatedAt, m.UpdatedAt = parseTime(leased), parseTime(synced), parseTime(code), parseTime(created), parseTime(updated)
			return m, nil
		}
	}
	return domain.Mailbox{}, ErrPoolEmpty
}

func containsTags(have, want []string) bool {
	set := map[string]bool{}
	for _, v := range have {
		set[strings.ToLower(strings.TrimSpace(v))] = true
	}
	for _, v := range want {
		if !set[v] {
			return false
		}
	}
	return true
}

func mailboxByIDTx(ctx context.Context, tx *sql.Tx, id string) (domain.Mailbox, error) {
	var m domain.Mailbox
	var tagJSON string
	var local, present, enabled int
	var leased, synced, code, created, updated sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,account_id,email,COALESCE(remote_anonymous_id,''),label,note,allocation_mode,purpose,tags_json,status,remote_status,local_enabled,remote_present,remote_enabled,last_leased_at,last_synced_at,last_code_at,created_at,updated_at FROM mailboxes WHERE id=?`, id).Scan(&m.ID, &m.AccountID, &m.Email, &m.RemoteAnonymousID, &m.Label, &m.Note, &m.AllocationMode, &m.Purpose, &tagJSON, &m.Status, &m.RemoteStatus, &local, &present, &enabled, &leased, &synced, &code, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	_ = json.Unmarshal([]byte(tagJSON), &m.Tags)
	m.LocalEnabled, m.RemotePresent, m.RemoteEnabled = local != 0, present != 0, enabled != 0
	m.LastLeasedAt, m.LastSyncedAt, m.LastCodeAt, m.CreatedAt, m.UpdatedAt = parseTime(leased), parseTime(synced), parseTime(code), parseTime(created), parseTime(updated)
	return m, err
}

func (s *Store) LeaseByCredentials(ctx context.Context, clientID, leaseID, token string) (domain.Lease, domain.Mailbox, error) {
	var l domain.Lease
	var created, updated, expires, released sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,mailbox_id,api_client_id,project,purpose,token_hash,idempotency_key,request_hash,status,created_at,updated_at,expires_at,released_at FROM leases WHERE id=? AND api_client_id=?`, leaseID, clientID).Scan(&l.ID, &l.MailboxID, &l.APIClientID, &l.Project, &l.Purpose, &l.TokenHash, &l.IdempotencyKey, &l.RequestHash, &l.Status, &created, &updated, &expires, &released)
	if errors.Is(err, sql.ErrNoRows) {
		return l, domain.Mailbox{}, ErrLeaseInactive
	}
	if err != nil {
		return l, domain.Mailbox{}, err
	}
	l.CreatedAt, l.UpdatedAt, l.ExpiresAt, l.ReleasedAt = parseTime(created), parseTime(updated), parseTime(expires), parseTime(released)
	if l.Status != domain.LeaseActive || !l.ExpiresAt.After(time.Now().UTC()) || !security.EqualHash(token, l.TokenHash) {
		return l, domain.Mailbox{}, ErrLeaseInactive
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return l, domain.Mailbox{}, err
	}
	defer tx.Rollback()
	m, err := mailboxByIDTx(ctx, tx, l.MailboxID)
	if err != nil {
		return l, m, err
	}
	return l, m, tx.Commit()
}

func (s *Store) RenewLease(ctx context.Context, clientID, leaseID, token string, ttlSeconds int) (domain.Lease, error) {
	l, _, err := s.LeaseByCredentials(ctx, clientID, leaseID, token)
	if err != nil {
		return l, err
	}
	now := time.Now().UTC()
	expires := now.Add(NormalizeLeaseTTL(ttlSeconds))
	max := l.CreatedAt.Add(24 * time.Hour)
	if expires.After(max) {
		expires = max
	}
	if !expires.After(now) {
		return l, ErrLeaseInactive
	}
	_, err = s.db.ExecContext(ctx, `UPDATE leases SET expires_at=?,updated_at=? WHERE id=? AND status='active'`, timeText(expires), timeText(now), l.ID)
	l.ExpiresAt, l.UpdatedAt = expires, now
	return l, err
}

func (s *Store) ReleaseLease(ctx context.Context, clientID, leaseID, token string, status string) error {
	l, m, err := s.LeaseByCredentials(ctx, clientID, leaseID, token)
	if err != nil {
		return err
	}
	if status == "" {
		status = domain.LeaseReleased
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE leases SET status=?,updated_at=?,released_at=? WHERE id=? AND status='active'`, status, timeText(now), timeText(now), l.ID); err != nil {
		return err
	}
	mailStatus := domain.MailboxAvailable
	if !m.RemotePresent {
		mailStatus = domain.MailboxMissing
	} else if !m.LocalEnabled || !m.RemoteEnabled {
		mailStatus = domain.MailboxDisabled
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET status=?,updated_at=? WHERE id=?`, mailStatus, timeText(now), m.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ExpireLeases(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,mailbox_id FROM leases WHERE status='active' AND expires_at<=?`, timeText(now))
	if err != nil {
		return 0, err
	}
	type pair struct{ id, mailbox string }
	var expired []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.mailbox); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, p)
	}
	rows.Close()
	for _, p := range expired {
		if _, err = tx.ExecContext(ctx, `UPDATE leases SET status='expired',updated_at=?,released_at=? WHERE id=?`, timeText(now), timeText(now), p.id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET status=CASE WHEN remote_present=0 THEN 'remote_missing' WHEN local_enabled=0 OR remote_enabled=0 THEN 'disabled' ELSE 'available' END,updated_at=? WHERE id=?`, timeText(now), p.mailbox); err != nil {
			return 0, err
		}
	}
	return len(expired), tx.Commit()
}

func (s *Store) FindCode(ctx context.Context, lease domain.Lease, after time.Time, keyword string, peek bool) (domain.Message, error) {
	if after.Before(lease.CreatedAt) {
		after = lease.CreatedAt
	}
	args := []any{lease.MailboxID, after.UTC().Format(messageTimeLayout)}
	where := `mailbox_id=? AND ` + messageTimeSQL + `>=? AND code<>''`
	if keyword != "" {
		where += ` AND (sender LIKE ? OR subject LIKE ?)`
		q := "%" + keyword + "%"
		args = append(args, q, q)
	}
	if !peek {
		where += ` AND consumed_at IS NULL`
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Message{}, err
	}
	defer tx.Rollback()
	var status, clientStatus string
	var expires sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT l.status,l.expires_at,c.status FROM leases l JOIN api_clients c ON c.id=l.api_client_id WHERE l.id=? AND l.mailbox_id=? AND l.api_client_id=? AND l.token_hash=?`, lease.ID, lease.MailboxID, lease.APIClientID, lease.TokenHash).Scan(&status, &expires, &clientStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Message{}, ErrLeaseInactive
	}
	if err != nil {
		return domain.Message{}, err
	}
	if status != domain.LeaseActive || clientStatus != "active" || !parseTime(expires).After(time.Now().UTC()) {
		return domain.Message{}, ErrLeaseInactive
	}
	var m domain.Message
	var received, synced, consumed sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,mailbox_id,remote_id,sender,subject,code,match_rule,confidence,received_at,synced_at,consumed_at,COALESCE(consumed_lease_id,'') FROM messages WHERE `+where+` ORDER BY `+messageTimeSQL+` DESC,id DESC LIMIT 1`, args...).Scan(&m.ID, &m.MailboxID, &m.RemoteID, &m.Sender, &m.Subject, &m.Code, &m.MatchRule, &m.Confidence, &received, &synced, &consumed, &m.ConsumedLeaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.ReceivedAt, m.SyncedAt, m.ConsumedAt = parseTime(received), parseTime(synced), parseTime(consumed)
	if !peek {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE messages SET consumed_at=?,consumed_lease_id=? WHERE id=? AND consumed_at IS NULL`, timeText(now), lease.ID, m.ID)
		if err != nil {
			return m, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return m, ErrNotFound
		}
		m.ConsumedAt, m.ConsumedLeaseID = now, lease.ID
	}
	return m, tx.Commit()
}
