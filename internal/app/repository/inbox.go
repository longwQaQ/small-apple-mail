package repository

import (
	"context"
	"database/sql"
	"strings"

	"small-apple-mail/internal/app/domain"
)

type InboxMessage struct {
	domain.Message
	MailboxEmail string `json:"mailbox_email"`
	AccountID    string `json:"account_id"`
	AccountName  string `json:"account_name"`
}

type InboxFilter struct {
	Query, AccountID, Kind string
	Page, PageSize         int
}

func (s *Store) ListInbox(ctx context.Context, filter InboxFilter) ([]InboxMessage, int, error) {
	where, args := []string{"1=1"}, []any{}
	if filter.AccountID != "" {
		where = append(where, "m.account_id=?")
		args = append(args, filter.AccountID)
	}
	if filter.Query != "" {
		where = append(where, "(msg.sender LIKE ? OR msg.subject LIKE ? OR m.email LIKE ? OR m.label LIKE ?)")
		q := "%" + filter.Query + "%"
		args = append(args, q, q, q, q)
	}
	switch filter.Kind {
	case "code":
		where = append(where, "msg.code<>''")
	case "link":
		where = append(where, "msg.login_url<>''")
	case "ordinary":
		where = append(where, "msg.code='' AND msg.login_url=''")
	}
	from := ` FROM messages msg JOIN mailboxes m ON m.id=msg.mailbox_id JOIN apple_accounts a ON a.id=m.account_id WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, filter.PageSize, (filter.Page-1)*filter.PageSize)
	rows, err := s.db.QueryContext(ctx, `SELECT msg.id,msg.mailbox_id,msg.sender,msg.subject,msg.code,msg.login_url,msg.received_at,msg.synced_at,m.email,m.account_id,COALESCE(NULLIF(a.display_name,''),a.apple_id)`+from+` ORDER BY `+messageTimeSQL+` DESC,msg.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]InboxMessage, 0, filter.PageSize)
	for rows.Next() {
		var m InboxMessage
		var received, synced sql.NullString
		if err = rows.Scan(&m.ID, &m.MailboxID, &m.Sender, &m.Subject, &m.Code, &m.LoginURL, &received, &synced, &m.MailboxEmail, &m.AccountID, &m.AccountName); err != nil {
			return nil, 0, err
		}
		m.ReceivedAt, m.SyncedAt = parseTime(received), parseTime(synced)
		out = append(out, m)
	}
	return out, total, rows.Err()
}
