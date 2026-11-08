package httpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
)

type mailboxReaderFunc func(context.Context, string) ([]protocol.ICloudRemoteMailbox, error)

func (f mailboxReaderFunc) ListPrivacyMailboxes(ctx context.Context, accountID string) ([]protocol.ICloudRemoteMailbox, error) {
	return f(ctx, accountID)
}

func TestSyncPreviewUsesSharedMailboxReader(t *testing.T) {
	var calledID string
	reader := mailboxReaderFunc(func(_ context.Context, accountID string) ([]protocol.ICloudRemoteMailbox, error) {
		calledID = accountID
		return []protocol.ICloudRemoteMailbox{{Email: "remote@example.invalid"}}, nil
	})
	server, store := testServer(t, reader)
	ctx := context.Background()
	a, err := store.CreateAccount(ctx, "preview@example.invalid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertConnection(ctx, a.ID, domain.ConnectionICloudWeb, domain.ConnectionHealthy, "www.icloud.com", `{}`, ""); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailbox-sync-previews", map[string]any{"account_ids": []string{a.ID}}, cookie, csrf)
	if res.StatusCode != 200 || calledID != a.ID {
		t.Fatalf("预览没有使用共享连接服务: status=%d error=%+v", res.StatusCode, out.Error)
	}
	var result struct {
		Token   string         `json:"confirmation_token"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(out.Data, &result); err != nil || result.Token == "" || result.Summary["created"] != 1 {
		t.Fatal("预览结果错误", err)
	}
}

func TestMailboxBatchRequiresCSRFAndReturnsPerItemResults(t *testing.T) {
	server, store := testServer(t)
	ctx := context.Background()
	a, _ := store.CreateAccount(ctx, "batch@example.invalid", "", "")
	m, _ := store.CreateMailbox(ctx, a.ID, "one@example.invalid", "", "", nil)
	body := map[string]any{"ids": []string{m.ID, "missing"}, "patch": map[string]any{"allocation_mode": "dedicated", "purpose": "长期用途", "tags": []string{"Work"}}}
	res, _ := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes/batch", body, nil, "")
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	cookie, csrf := login(t, server)
	res, _ = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes/batch", body, cookie, "")
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes/batch", body, cookie, csrf)
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	var result struct {
		Items []struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out.Data, &result); err != nil || len(result.Items) != 2 || !result.Items[0].Success || result.Items[1].Success || result.Items[1].Error == "" {
		t.Fatal(result, err)
	}
	saved, _ := store.Mailbox(ctx, m.ID)
	if saved.AllocationMode != "dedicated" || saved.Purpose != "长期用途" {
		t.Fatal(saved)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes?query=work&allocation_mode=dedicated", nil, cookie, "")
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(out.Data, &list)
	if res.StatusCode != 200 || len(list.Items) != 1 {
		t.Fatal(res.StatusCode, string(out.Data))
	}
}

func TestAdminMailboxResultWaitsForNewOfflineLink(t *testing.T) {
	server, store := testServer(t)
	ctx := context.Background()
	a, _ := store.CreateAccount(ctx, "result@example.invalid", "", "")
	m, _ := store.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	cookie, _ := login(t, server)
	now := time.Now().UTC()
	_, _ = store.SaveMessages(ctx, []repository.MessageInput{{MailboxID: m.ID, RemoteID: "old", Code: "111111", ReceivedAt: now.Add(-time.Hour)}})
	path := server.URL + "/api/admin/mailboxes/" + m.ID + "/result?" + url.Values{"after": []string{now.Format(time.RFC3339Nano)}, "kind": []string{"link"}, "wait_seconds": []string{"2"}, "keyword": []string{"Claude"}}.Encode()
	done := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, err := store.SaveMessages(ctx, []repository.MessageInput{{MailboxID: m.ID, RemoteID: "new", Subject: "Sign in to Claude", LoginURL: "https://claude.ai/magic-link?token=local-fixture", ReceivedAt: time.Now()}})
		done <- err
	}()
	res, out := doJSON(t, server.Client(), "GET", path, nil, cookie, "")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var data struct {
		Message struct {
			LoginURL string `json:"login_url"`
			Code     string `json:"code"`
		} `json:"message"`
		Waiting bool `json:"waiting"`
	}
	_ = json.Unmarshal(out.Data, &data)
	if res.StatusCode != 200 || data.Waiting || data.Message.LoginURL == "" || data.Message.Code != "" {
		t.Fatal(res.StatusCode, string(out.Data))
	}
	res, _ = doJSON(t, server.Client(), "GET", path, nil, nil, "")
	if res.StatusCode != 401 {
		t.Fatal("结果读取必须认证", res.StatusCode)
	}
}

func TestRetryJobOnlyRepeatsFailedSyncAccounts(t *testing.T) {
	server, store := testServer(t)
	ctx := context.Background()
	job, _ := store.CreateJob(ctx, "mailbox.remote_sync", `{"account_ids":["ok","bad"]}`, 10)
	_ = store.AddJobItem(ctx, job.ID, "ok", "正常账号", "succeeded", "完成", 2)
	_ = store.AddJobItem(ctx, job.ID, "bad", "失败账号", "failed", "临时故障", 2)
	_ = store.CompleteJob(ctx, job.ID, "partially_succeeded", "", "")
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/jobs/"+job.ID+"/retry", nil, cookie, csrf)
	if res.StatusCode != 202 {
		t.Fatal(res.StatusCode)
	}
	var next struct {
		Payload string `json:"payload"`
	}
	_ = json.Unmarshal(out.Data, &next)
	var payload struct {
		IDs []string `json:"account_ids"`
	}
	_ = json.Unmarshal([]byte(next.Payload), &payload)
	if len(payload.IDs) != 1 || payload.IDs[0] != "bad" {
		t.Fatal(next)
	}
	creation, _ := store.CreateJob(ctx, "mailbox.create", `{}`, 10)
	_ = store.CompleteJob(ctx, creation.ID, "failed", "", "")
	res, _ = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/jobs/"+creation.ID+"/retry", nil, cookie, csrf)
	if res.StatusCode != 400 {
		t.Fatal("创建任务不应被通用重试重放", res.StatusCode)
	}
}
