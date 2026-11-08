package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
)

func TestAdminLoginLinks(t *testing.T) {
	server, store := testServer(t)
	cookie, _ := login(t, server)
	ctx := context.Background()
	account, err := store.CreateAccount(ctx, "owner@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := store.CreateMailbox(ctx, account.ID, "alias@icloud.com", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	const link = "https://claude.ai/magic-link#synthetic:test"
	if _, err = store.SaveMessages(ctx, []repository.MessageInput{{MailboxID: mailbox.ID, RemoteID: "imap:1", LoginURL: link, Body: "Sign in", ReceivedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	res, out := doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/messages", nil, cookie, "")
	var page struct {
		Items []domain.Message `json:"items"`
	}
	if res.StatusCode != http.StatusOK || json.Unmarshal(out.Data, &page) != nil || len(page.Items) != 1 || page.Items[0].LoginURL != link {
		t.Fatal("收件列表未提供登录链接")
	}
	endpoint := server.URL + "/api/admin/mailboxes/" + mailbox.ID + "/messages/" + page.Items[0].ID
	res, out = doJSON(t, server.Client(), http.MethodGet, endpoint, nil, cookie, "")
	var detail domain.Message
	if res.StatusCode != http.StatusOK || json.Unmarshal(out.Data, &detail) != nil || detail.LoginURL != link || detail.Body != "Sign in" {
		t.Fatal("邮件详情未提供登录链接")
	}
	res, _ = doJSON(t, server.Client(), http.MethodGet, endpoint, nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatal("未登录请求不得读取登录链接")
	}
}
