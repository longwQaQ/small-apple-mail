package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

type RemoteMailboxInput struct {
	AllocationMode string
	Purpose        string
	AnonymousID    string
	Email          string
	Label          string
	Note           string
	Active         bool
	CreatedAt      time.Time
}

func (s *Store) SyncRemoteMailboxes(ctx context.Context, accountID string, items []RemoteMailboxInput) (int, error) {
	return s.syncRemoteMailboxes(ctx, accountID, items, true)
}

func (s *Store) syncRemoteMailboxes(ctx context.Context, accountID string, items []RemoteMailboxInput, markMissing bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowText()
	if markMissing {
		if _, err = tx.ExecContext(ctx, `UPDATE mailboxes SET remote_present=0,remote_status='missing',status=CASE WHEN status='leased' THEN status ELSE 'remote_missing' END,updated_at=? WHERE account_id=?`, now, accountID); err != nil {
			return 0, err
		}
	}
	seenEmails := make(map[string]struct{}, len(items))
	seenAnonymousIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		email := strings.ToLower(strings.TrimSpace(item.Email))
		anonymousID := strings.TrimSpace(item.AnonymousID)
		if email == "" {
			return 0, fmt.Errorf("远端邮箱地址不能为空")
		}
		if _, exists := seenEmails[email]; exists {
			return 0, fmt.Errorf("远端快照包含重复邮箱 %s", email)
		}
		seenEmails[email] = struct{}{}
		if anonymousID != "" {
			if _, exists := seenAnonymousIDs[anonymousID]; exists {
				return 0, fmt.Errorf("远端快照包含重复标识 %s", anonymousID)
			}
			seenAnonymousIDs[anonymousID] = struct{}{}
		}
		mailboxID, err := remoteMailboxMatch(ctx, tx, accountID, anonymousID, email)
		if err != nil {
			return 0, err
		}
		remoteStatus := "disabled"
		status := domain.MailboxDisabled
		if item.Active {
			remoteStatus = "active"
			status = domain.MailboxAvailable
		}
		created := item.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if mailboxID == "" {
			mode := item.AllocationMode
			if mode == "" {
				mode = "shared"
			}
			if mode != "shared" && mode != "dedicated" {
				return 0, fmt.Errorf("无效邮箱用途类型")
			}
			mailboxID, err = security.ID("mbx")
			if err != nil {
				return 0, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes(id,account_id,email,remote_anonymous_id,label,note,allocation_mode,purpose,tags_json,status,remote_status,local_enabled,remote_present,remote_enabled,last_synced_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'[]',?,?,1,1,?,?,?,?)`, mailboxID, accountID, email, anonymousID, item.Label, item.Note, mode, item.Purpose, status, remoteStatus, item.Active, now, timeText(created), now)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET email=?,remote_anonymous_id=?,remote_present=1,remote_enabled=?,remote_status=?,last_synced_at=?,updated_at=?,status=CASE WHEN status='leased' THEN status WHEN local_enabled=0 THEN 'disabled' ELSE ? END WHERE id=? AND account_id=?`, email, anonymousID, item.Active, remoteStatus, now, now, status, mailboxID, accountID)
		}
		if err != nil {
			return 0, fmt.Errorf("同步邮箱 %s: %w", email, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(items), nil
}

func remoteMailboxMatch(ctx context.Context, tx *sql.Tx, accountID, anonymousID, email string) (string, error) {
	if anonymousID != "" {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE account_id=? AND remote_anonymous_id=?`, accountID, anonymousID).Scan(&id)
		if err == nil {
			var emailOwnerID, emailOwnerAccountID string
			err = tx.QueryRowContext(ctx, `SELECT id,account_id FROM mailboxes WHERE email=? COLLATE NOCASE`, email).Scan(&emailOwnerID, &emailOwnerAccountID)
			if err == nil && emailOwnerID != id {
				return "", fmt.Errorf("远端邮箱 %s 与账号 %s 的已有邮箱冲突", email, emailOwnerAccountID)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", err
			}
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var id, ownerAccountID string
	err := tx.QueryRowContext(ctx, `SELECT id,account_id FROM mailboxes WHERE email=? COLLATE NOCASE`, email).Scan(&id, &ownerAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if ownerAccountID != accountID {
		return "", fmt.Errorf("远端邮箱 %s 已属于其他账号 %s", email, ownerAccountID)
	}
	return id, nil
}

func (s *Store) SaveCreatedRemoteMailbox(ctx context.Context, accountID string, item RemoteMailboxInput) (domain.Mailbox, error) {
	if _, err := s.SyncRemoteMailboxesNoMissing(ctx, accountID, []RemoteMailboxInput{item}); err != nil {
		return domain.Mailbox{}, err
	}
	rows, _, err := s.ListMailboxes(ctx, item.Email, "", accountID, "", 10)
	if err != nil || len(rows) == 0 {
		return domain.Mailbox{}, err
	}
	return rows[0], nil
}
func (s *Store) SyncRemoteMailboxesNoMissing(ctx context.Context, accountID string, items []RemoteMailboxInput) (int, error) {
	return s.syncRemoteMailboxes(ctx, accountID, items, false)
}
