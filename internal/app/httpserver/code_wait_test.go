package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
)

func TestCodeWaitRejectsRevokedCredentials(t *testing.T) {
	for _, action := range []string{"release", "expire", "disable-client", "rotate-client", "rotate-mailbox"} {
		t.Run(action, func(t *testing.T) {
			server, store := testServer(t)
			ctx := context.Background()
			account, err := store.CreateAccount(ctx, "owner@icloud.com", "", "")
			if err != nil {
				t.Fatal(err)
			}
			mailbox, err := store.CreateMailbox(ctx, account.ID, "alias@icloud.com", "", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			client, key, err := store.CreateAPIClient(ctx, "test", []string{"codes:read"})
			if err != nil {
				t.Fatal(err)
			}
			lease, _, token, _, err := store.CreateLease(ctx, client, repository.LeaseRequest{IdempotencyKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			endpoint := server.URL + "/api/leases/" + lease.ID + "/code?wait_seconds=3&after=" + url.QueryEscape(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
			wantStatus := http.StatusUnauthorized
			if action == "release" || action == "expire" {
				wantStatus = http.StatusNotFound
			}
			if action == "rotate-mailbox" {
				mailboxToken, err := store.RotateMailboxAPIToken(ctx, mailbox.ID)
				if err != nil {
					t.Fatal(err)
				}
				endpoint = server.URL + "/api/mailboxes/" + mailbox.ID + "/code?wait_seconds=3&key=" + url.QueryEscape(mailboxToken)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Lease-Token", token)
			type result struct {
				response *http.Response
				err      error
			}
			completed := make(chan result, 1)
			go func() { response, err := server.Client().Do(req); completed <- result{response, err} }()
			// 同步任务入库说明请求已验证凭据，进入等待邮件的阶段。
			deadline := time.Now().Add(2 * time.Second)
			for {
				jobs, err := store.ListJobs(ctx, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(jobs) > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("取码请求未进入等待阶段")
				}
				time.Sleep(5 * time.Millisecond)
			}
			switch action {
			case "release":
				err = store.ReleaseLease(ctx, client.ID, lease.ID, token, domain.LeaseReleased)
			case "expire":
				_, err = store.DB().ExecContext(ctx, "UPDATE leases SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), lease.ID)
			case "disable-client":
				err = store.SetAPIClientStatus(ctx, client.ID, "disabled")
			case "rotate-client":
				_, err = store.RotateAPIClient(ctx, client.ID)
			case "rotate-mailbox":
				_, err = store.RotateMailboxAPIToken(ctx, mailbox.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.SaveMessages(ctx, []repository.MessageInput{{MailboxID: mailbox.ID, RemoteID: "imap:new", Code: "123456", ReceivedAt: time.Now().UTC()}}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-completed:
				if got.err != nil {
					t.Fatal(got.err)
				}
				defer got.response.Body.Close()
				if got.response.StatusCode != wantStatus {
					t.Fatalf("凭据失效后仍返回验证码: status=%d want=%d", got.response.StatusCode, wantStatus)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("取码请求未结束")
			}
			var consumed int
			if err = store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE consumed_at IS NOT NULL").Scan(&consumed); err != nil || consumed != 0 {
				t.Fatalf("无效请求不应消费验证码: consumed=%d err=%v", consumed, err)
			}
		})
	}
}

func TestMailboxCodeRequestsShareSyncJob(t *testing.T) {
	server, store := testServer(t)
	ctx := context.Background()
	account, err := store.CreateAccount(ctx, "owner@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := store.CreateMailbox(ctx, account.ID, "alias@icloud.com", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.RotateMailboxAPIToken(ctx, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		response, err := server.Client().Get(server.URL + "/api/mailboxes/" + mailbox.ID + "/code?key=" + url.QueryEscape(token))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("status=%d", response.StatusCode)
		}
	}
	jobs, err := store.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("重复创建同步任务: jobs=%d err=%v", len(jobs), err)
	}
}
