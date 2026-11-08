package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"small-apple-mail/internal/app/repository"
)

func TestUnifiedInbox(t *testing.T) {
	server, store := testServer(t)
	cookie, _ := login(t, server)
	ctx := context.Background()
	a, err := store.CreateAccount(ctx, "first@example.invalid", "账号一", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateAccount(ctx, "second@example.invalid", "账号二", "")
	if err != nil {
		t.Fatal(err)
	}
	ma, err := store.CreateMailbox(ctx, a.ID, "alpha@example.invalid", "工作邮箱", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := store.CreateMailbox(ctx, b.ID, "beta@example.invalid", "个人邮箱", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	_, err = store.SaveMessages(ctx, []repository.MessageInput{
		{MailboxID: ma.ID, RemoteID: "code", Sender: "security@example.invalid", Subject: "登录验证码", Code: "123456", Body: "不应在列表中返回正文", ReceivedAt: now},
		{MailboxID: mb.ID, RemoteID: "link", Sender: "login@example.invalid", Subject: "Claude 登录链接", LoginURL: "https://claude.ai/magic-link#synthetic:example", ReceivedAt: now.Add(time.Nanosecond)},
		{MailboxID: mb.ID, RemoteID: "ordinary", Sender: "news@example.invalid", Subject: "产品更新", ReceivedAt: now.Add(-time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var firstID string
	for _, tc := range []struct {
		name, query  string
		total, count int
		email        string
	}{
		{"全部邮箱", "", 3, 3, "beta@example.invalid"},
		{"第一分页", "?page_size=1", 3, 1, "beta@example.invalid"},
		{"第二分页", "?page_size=1&page=2", 3, 1, "alpha@example.invalid"},
		{"账号筛选", "?account_id=" + a.ID, 1, 1, "alpha@example.invalid"},
		{"验证码", "?kind=code", 1, 1, "alpha@example.invalid"},
		{"登录链接", "?kind=link", 1, 1, "beta@example.invalid"},
		{"普通邮件", "?kind=ordinary", 1, 1, "beta@example.invalid"},
		{"邮箱搜索", "?query=alpha", 1, 1, "alpha@example.invalid"},
		{"主题搜索", "?query=产品更新", 1, 1, "beta@example.invalid"},
		{"发件人搜索", "?query=security", 1, 1, "alpha@example.invalid"},
		{"空结果", "?query=不存在", 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, out := doJSON(t, server.Client(), "GET", server.URL+"/api/admin/messages"+tc.query, nil, cookie, "")
			if res.StatusCode != 200 {
				t.Fatalf("status=%d error=%+v", res.StatusCode, out.Error)
			}
			var data struct {
				Items []repository.InboxMessage `json:"items"`
				Total int                       `json:"total"`
			}
			if err := json.Unmarshal(out.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Total != tc.total || len(data.Items) != tc.count {
				t.Fatalf("total=%d count=%d", data.Total, len(data.Items))
			}
			if tc.count > 0 {
				m := data.Items[0]
				if m.MailboxEmail != tc.email || m.AccountName == "" || m.Body != "" {
					t.Fatalf("邮箱归属或正文投影不正确: %+v", m)
				}
				if tc.name == "验证码" {
					firstID = m.ID
				}
			}
		})
	}
	res, out := doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes/"+ma.ID+"/messages/"+firstID, nil, cookie, "")
	if res.StatusCode != 200 || !json.Valid(out.Data) {
		t.Fatalf("详情读取失败: %d", res.StatusCode)
	}
	res, _ = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/messages?kind=invalid", nil, cookie, "")
	if res.StatusCode != 400 {
		t.Fatalf("无效类型应拒绝: %d", res.StatusCode)
	}
	res, _ = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/messages", nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名请求应拒绝: %d", res.StatusCode)
	}
}
