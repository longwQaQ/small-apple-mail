package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
)

func TestFindCodeRejectsStaleLease(t *testing.T) {
	for _, peek := range []bool{false, true} {
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
		client, _, err := s.CreateAPIClient(ctx, "test", []string{"codes:read"})
		if err != nil {
			t.Fatal(err)
		}
		lease, _, token, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "old"})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.ReleaseLease(ctx, client.ID, lease.ID, token, domain.LeaseReleased); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err = s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "new"}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: mailbox.ID, RemoteID: "imap:1", Code: "123456", ReceivedAt: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.FindCode(ctx, lease, time.Now().Add(-time.Minute), "", peek); !errors.Is(err, ErrLeaseInactive) {
			t.Fatalf("旧租约取得了新租约的验证码: peek=%v err=%v", peek, err)
		}
		var consumed int
		if err = s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE consumed_at IS NOT NULL").Scan(&consumed); err != nil || consumed != 0 {
			t.Fatalf("旧租约消费了验证码: consumed=%d err=%v", consumed, err)
		}
	}
}
