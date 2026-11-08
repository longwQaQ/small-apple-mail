package repository

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginLinkPersistenceAndResync(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "owner@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := s.CreateMailbox(ctx, account.ID, "alias@icloud.com", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	input := MessageInput{MailboxID: mailbox.ID, RemoteID: "imap:1", Subject: "Your secure link to Claude is here", Body: "Sign in", ReceivedAt: time.Now().UTC()}
	if n, err := s.SaveMessages(ctx, []MessageInput{input}); err != nil || n != 1 {
		t.Fatalf("首次保存失败: n=%d err=%v", n, err)
	}
	rows, err := s.ListMessages(ctx, mailbox.ID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取邮件失败: err=%v", err)
	}
	id := rows[0].ID
	if _, err = s.db.ExecContext(ctx, "UPDATE messages SET consumed_at=? WHERE id=?", nowText(), id); err != nil {
		t.Fatal(err)
	}
	input.LoginURL = "https://claude.ai/magic-link#synthetic:test"
	if n, err := s.SaveMessages(ctx, []MessageInput{input}); err != nil || n != 1 {
		t.Fatalf("重复同步未补充登录链接: n=%d err=%v", n, err)
	}
	if n, err := s.SaveMessages(ctx, []MessageInput{input}); err != nil || n != 0 {
		t.Fatalf("重复邮件不应重复写入: n=%d err=%v", n, err)
	}
	detail, err := s.Message(ctx, mailbox.ID, id)
	if err != nil || detail.LoginURL != input.LoginURL || detail.ConsumedAt.IsZero() || detail.Body != "Sign in" {
		t.Fatal("补充链接改变了原有邮件状态")
	}
	page, total, err := s.ListMessagesPage(ctx, mailbox.ID, 1, 10)
	if err != nil || total != 1 || len(page) != 1 || page[0].LoginURL != input.LoginURL || page[0].Body != "" {
		t.Fatal("邮件列表未返回登录链接")
	}
	mailboxes, _, err := s.ListMailboxes(ctx, "", "", account.ID, "", 10)
	if err != nil || len(mailboxes) != 1 || mailboxes[0].LastLoginURL != input.LoginURL || !mailboxes[0].LastLoginURLAt.Equal(input.ReceivedAt) {
		t.Fatal("邮箱列表未返回最近登录链接")
	}
	if _, err = s.FindMailboxCode(ctx, mailbox.ID, input.ReceivedAt.Add(-time.Minute), ""); err != ErrNotFound {
		t.Fatal("登录链接邮件不应改变验证码 API 行为")
	}
}

func TestOpenMigratesV2LoginLinks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v2 := strings.Replace(legacyV3Schema(), "('schema_version','3')", "('schema_version','2')", 1)
	v2 = strings.Replace(v2, " login_url TEXT NOT NULL DEFAULT '',", "", 1)
	if _, err = db.ExecContext(ctx, v2); err != nil {
		t.Fatal(err)
	}
	now := nowText()
	if _, err = db.ExecContext(ctx, `INSERT INTO apple_accounts(id,apple_id,status,created_at,updated_at) VALUES('acc','v2@icloud.com','healthy',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO mailboxes(id,account_id,email,status,created_at,updated_at) VALUES('mbx','acc','alias@icloud.com','available',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,remote_id,code,body,received_at,synced_at,consumed_at) VALUES('msg','mbx','imap:1','123456','历史正文',?,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if version, err := s.SchemaVersion(ctx); err != nil || version != schemaVersion {
			t.Fatalf("version=%d err=%v", version, err)
		}
		message, err := s.Message(ctx, "mbx", "msg")
		if err != nil || message.Code != "123456" || message.Body != "历史正文" || message.LoginURL != "" || message.ConsumedAt.IsZero() {
			t.Fatal("升级或重新打开数据库后，历史邮件或消费状态改变")
		}
		if err = s.IntegrityCheck(ctx); err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
