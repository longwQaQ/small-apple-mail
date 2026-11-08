package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

// RotateMailboxAPIToken 创建新的单邮箱访问凭据。数据库只保存 Token 哈希，
// 因此轮换后，上一次复制的邮箱 API 地址会立即失效。
func (s *Store) RotateMailboxAPIToken(ctx context.Context, mailboxID string) (string, error) {
	if _, err := s.Mailbox(ctx, mailboxID); err != nil {
		return "", err
	}
	token, err := security.RandomToken("sam_mailbox_", 32)
	if err != nil {
		return "", err
	}
	now := nowText()
	_, err = s.db.ExecContext(ctx, `INSERT INTO mailbox_api_credentials(mailbox_id,token_hash,created_at,updated_at) VALUES(?,?,?,?) ON CONFLICT(mailbox_id) DO UPDATE SET token_hash=excluded.token_hash,updated_at=excluded.updated_at`, mailboxID, security.HashToken(token), now, now)
	return token, err
}

func (s *Store) MailboxByAPIToken(ctx context.Context, mailboxID, token string) (domain.Mailbox, error) {
	if token == "" {
		return domain.Mailbox{}, ErrNotFound
	}
	var matched string
	err := s.db.QueryRowContext(ctx, `SELECT mailbox_id FROM mailbox_api_credentials WHERE mailbox_id=? AND token_hash=?`, mailboxID, security.HashToken(token)).Scan(&matched)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Mailbox{}, ErrNotFound
	}
	if err != nil {
		return domain.Mailbox{}, err
	}
	return s.Mailbox(ctx, matched)
}

func (s *Store) FindMailboxCode(ctx context.Context, mailboxID string, after time.Time, keyword string) (domain.Message, error) {
	args := []any{mailboxID, after.UTC().Format(messageTimeLayout)}
	where := `mailbox_id=? AND ` + messageTimeSQL + `>=? AND code<>''`
	if keyword != "" {
		where += ` AND (sender LIKE ? OR subject LIKE ?)`
		q := "%" + keyword + "%"
		args = append(args, q, q)
	}
	var message domain.Message
	var received, synced, consumed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,mailbox_id,remote_id,sender,subject,code,match_rule,confidence,received_at,synced_at,consumed_at,COALESCE(consumed_lease_id,'') FROM messages WHERE `+where+` ORDER BY `+messageTimeSQL+` DESC,id DESC LIMIT 1`, args...).Scan(&message.ID, &message.MailboxID, &message.RemoteID, &message.Sender, &message.Subject, &message.Code, &message.MatchRule, &message.Confidence, &received, &synced, &consumed, &message.ConsumedLeaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return message, ErrNotFound
	}
	if err != nil {
		return message, err
	}
	message.ReceivedAt, message.SyncedAt, message.ConsumedAt = parseTime(received), parseTime(synced), parseTime(consumed)
	return message, nil
}
