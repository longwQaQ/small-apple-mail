package repository

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"small-apple-mail/internal/app/security"
)

type MessageInput struct {
	MailboxID, RemoteID, Sender, Subject, Code, LoginURL, MatchRule, Body string
	Confidence                                                            float64
	ReceivedAt                                                            time.Time
}

const messageTimeLayout = "2006-01-02T15:04:05.000000000Z"

// 写入时间均为 UTC；补齐旧记录的小数位后比较，保留纳秒精度。
// SQLite 的 julianday/unixepoch 会丢失精度，不适用于租约取码边界。
const messageTimeSQL = `(substr(received_at,1,19) || '.' || substr(rtrim(substr(received_at,21),'Z') || '000000000',1,9) || 'Z')`

func (s *Store) SaveMessages(ctx context.Context, items []MessageInput) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	saved, err := saveMessagesTx(ctx, tx, items)
	if err != nil {
		return 0, err
	}
	return saved, tx.Commit()
}

func saveMessagesTx(ctx context.Context, tx *sql.Tx, items []MessageInput) (int, error) {
	saved := 0
	for _, m := range items {
		id, _ := security.ID("msg")
		received := m.ReceivedAt
		if received.IsZero() {
			received = time.Now().UTC()
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,remote_id,sender,subject,code,login_url,match_rule,confidence,body,received_at,synced_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(mailbox_id,remote_id) DO UPDATE SET login_url=excluded.login_url WHERE messages.login_url='' AND excluded.login_url<>''`, id, m.MailboxID, m.RemoteID, m.Sender, m.Subject, m.Code, m.LoginURL, m.MatchRule, m.Confidence, m.Body, timeText(received), nowText())
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		saved += int(n)
		if n > 0 && m.Code != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET last_code_at=CASE WHEN last_code_at IS NULL OR last_code_at<? THEN ? ELSE last_code_at END WHERE id=?`, timeText(received), timeText(received), m.MailboxID); err != nil {
				return 0, err
			}
		}
	}
	return saved, nil
}

func (s *Store) CleanupMessages(ctx context.Context) (int, error) {
	days := 30
	if value, err := s.Setting(ctx, "message_retention_days"); err == nil {
		if n, parseErr := strconv.Atoi(value); parseErr == nil && n >= 1 {
			days = n
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE received_at<?`, timeText(now.Add(-time.Duration(days)*24*time.Hour)))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET last_code_at=(SELECT MAX(received_at) FROM messages WHERE messages.mailbox_id=mailboxes.id AND messages.code<>'')`); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

func (s *Store) RemoteVerificationMessageIDs(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT msg.remote_id FROM messages msg JOIN mailboxes m ON m.id=msg.mailbox_id WHERE m.account_id=? AND msg.code<>'' AND msg.remote_id LIKE 'imap:%' AND msg.remote_cleaned_at IS NULL`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) MarkRemoteVerificationMessagesCleaned(ctx context.Context, accountID string, remoteIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowText()
	for _, remoteID := range remoteIDs {
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET remote_cleaned_at=? WHERE remote_id=? AND code<>'' AND mailbox_id IN (SELECT id FROM mailboxes WHERE account_id=?)`, now, remoteID, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
