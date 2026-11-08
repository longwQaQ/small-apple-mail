package providers

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
)

func TestMessageSyncCatchesUpPagesAndResumesPersistedCursor(t *testing.T) {
	ctx := context.Background()
	s, err := repository.Open(ctx, filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateAccount(ctx, "owner@example.invalid", "", "")
	m, _ := s.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	_, _ = s.UpsertConnection(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "", `{"email":"owner@example.invalid","app_password":"fixture"}`, "")
	calls := 0
	fetch := func(_ context.Context, _ protocol.LoginState, _ []protocol.Mailbox, _ time.Time, cursor protocol.IMAPCursor) (protocol.IMAPSyncResult, error) {
		expected := []string{"", "200", "400", "401"}
		if calls >= len(expected) || cursor.LastUID != expected[calls] {
			t.Fatalf("错误的分页游标 call=%d cursor=%+v", calls, cursor)
		}
		calls++
		last := fmt.Sprint(calls * 200)
		if calls >= 3 {
			last = "401"
		}
		rows := map[string][]protocol.ICloudSyncedMessage{}
		if calls <= 3 {
			rows[m.ID] = []protocol.ICloudSyncedMessage{{RemoteID: "imap:" + last, Subject: "Verification code", Body: "code: 654321", ReceivedAt: time.Now()}}
		}
		return protocol.IMAPSyncResult{Cursor: protocol.IMAPCursor{UIDValidity: "7", LastUID: last}, HasMore: calls < 3, MessagesByMailbox: rows}, nil
	}
	n, err := syncAccountMessages(ctx, s, a.ID, 0, fetch)
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	n, err = syncAccountMessages(ctx, s, a.ID, 0, fetch)
	if err != nil || n != 0 || calls != 4 {
		t.Fatal(n, calls, err)
	}
	rows, _ := s.ListMessages(ctx, m.ID, 10)
	if len(rows) != 3 {
		t.Fatal(rows)
	}
}
