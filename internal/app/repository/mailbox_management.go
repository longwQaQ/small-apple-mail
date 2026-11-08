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
)

type MailboxFilter struct {
	Query, Status, AccountID, Cursor, Mode, Tag string
	Limit                                       int
	WithResult, Unused                          bool
}

type MailboxPatch struct {
	Label          *string   `json:"label"`
	Note           *string   `json:"note"`
	AllocationMode *string   `json:"allocation_mode"`
	Purpose        *string   `json:"purpose"`
	LocalEnabled   *bool     `json:"local_enabled"`
	Tags           *[]string `json:"tags"`
}

func (s *Store) PatchMailbox(ctx context.Context, id string, patch MailboxPatch) (domain.Mailbox, error) {
	sets, args := []string{"updated_at=?"}, []any{nowText()}
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{{"label", patch.Label, 120}, {"note", patch.Note, 2000}, {"purpose", patch.Purpose, 120}} {
		if field.value == nil {
			continue
		}
		value := strings.TrimSpace(*field.value)
		if len([]rune(value)) > field.max {
			return domain.Mailbox{}, fmt.Errorf("%s 内容过长", field.name)
		}
		sets = append(sets, field.name+"=?")
		args = append(args, value)
	}
	if patch.AllocationMode != nil {
		if *patch.AllocationMode != "shared" && *patch.AllocationMode != "dedicated" {
			return domain.Mailbox{}, fmt.Errorf("无效邮箱用途类型")
		}
		sets = append(sets, "allocation_mode=?")
		args = append(args, *patch.AllocationMode)
	}
	if patch.Tags != nil {
		tags, err := normalizeTags(*patch.Tags)
		if err != nil {
			return domain.Mailbox{}, err
		}
		data, _ := json.Marshal(tags)
		sets = append(sets, "tags_json=?")
		args = append(args, string(data))
	}
	if patch.LocalEnabled != nil {
		sets = append(sets, "local_enabled=?", "status=CASE WHEN status='leased' THEN status WHEN ?=0 THEN 'disabled' WHEN remote_present=0 THEN 'remote_missing' WHEN remote_enabled=0 THEN 'disabled' ELSE 'available' END")
		args = append(args, *patch.LocalEnabled, *patch.LocalEnabled)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Mailbox{}, err
	}
	defer tx.Rollback()
	if patch.AllocationMode != nil {
		var active int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM leases l JOIN mailboxes m ON m.id=l.mailbox_id WHERE l.mailbox_id=? AND l.status='active' AND m.allocation_mode<>?", id, *patch.AllocationMode).Scan(&active); err != nil {
			return domain.Mailbox{}, err
		}
		if active > 0 {
			return domain.Mailbox{}, fmt.Errorf("存在活动租约，请释放后再调整用途类型")
		}
	}
	args = append(args, id)
	result, err := tx.ExecContext(ctx, "UPDATE mailboxes SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
	if err != nil {
		return domain.Mailbox{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return domain.Mailbox{}, err
	}
	if n == 0 {
		return domain.Mailbox{}, ErrNotFound
	}
	mailbox, err := mailboxByIDTx(ctx, tx, id)
	if err != nil {
		return mailbox, err
	}
	return mailbox, tx.Commit()
}

// FindMailboxResult 仅查询已经保存的结果，不访问邮件中的任何资源。
func (s *Store) FindMailboxResult(ctx context.Context, mailboxID, kind, keyword string, after time.Time) (domain.Message, error) {
	condition := "(code<>'' OR login_url<>'')"
	switch kind {
	case "code":
		condition = "code<>''"
	case "link":
		condition = "login_url<>''"
	case "", "all":
	default:
		return domain.Message{}, fmt.Errorf("无效结果类型")
	}
	var m domain.Message
	var received, synced sql.NullString
	query := `SELECT id,mailbox_id,sender,subject,code,login_url,received_at,synced_at FROM messages WHERE mailbox_id=? AND ` + messageTimeSQL + `>=? AND ` + condition
	args := []any{mailboxID, after.UTC().Format(messageTimeLayout)}
	if keyword != "" {
		query += " AND (sender LIKE ? OR subject LIKE ?)"
		args = append(args, "%"+keyword+"%", "%"+keyword+"%")
	}
	err := s.db.QueryRowContext(ctx, query+" ORDER BY "+messageTimeSQL+" DESC,id DESC LIMIT 1", args...).Scan(&m.ID, &m.MailboxID, &m.Sender, &m.Subject, &m.Code, &m.LoginURL, &received, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	m.ReceivedAt, m.SyncedAt = parseTime(received), parseTime(synced)
	return m, err
}
