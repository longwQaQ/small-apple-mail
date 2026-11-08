package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"small-apple-mail/internal/app/config"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/web"
)

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func testServer(t *testing.T, readers ...MailboxReader) (*httptest.Server, *repository.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "small-apple-mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.BootstrapAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	var reader MailboxReader
	if len(readers) > 0 {
		reader = readers[0]
	}
	server := httptest.NewServer(New(config.Config{SessionIdleMinutes: 60, SessionAbsoluteHours: 24}, store, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, reader))
	t.Cleanup(func() { server.Close(); store.Close() })
	return server, store
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, cookie *http.Cookie, csrf string) (*http.Response, envelope) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, url, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out envelope
	if res.StatusCode != 204 {
		_ = json.NewDecoder(res.Body).Decode(&out)
	}
	res.Body.Close()
	return res, out
}

func login(t *testing.T, server *httptest.Server) (*http.Cookie, string) {
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/session", map[string]any{"username": "admin", "password": "correct horse battery staple"}, nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("login=%d error=%+v", res.StatusCode, out.Error)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%d", len(cookies))
	}
	var data struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(out.Data, &data); err != nil || data.CSRF == "" {
		t.Fatalf("csrf=%q err=%v", data.CSRF, err)
	}
	return cookies[0], data.CSRF
}

func TestSessionCSRFRemainsValidAcrossTabs(t *testing.T) {
	server, _ := testServer(t)
	cookie, first := login(t, server)
	readSession := func() string {
		t.Helper()
		res, out := doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/session", nil, cookie, "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("session=%d error=%+v", res.StatusCode, out.Error)
		}
		var data struct {
			CSRF string `json:"csrf_token"`
		}
		if err := json.Unmarshal(out.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.CSRF
	}
	if second, third := readSession(), readSession(); first != second || second != third {
		t.Fatalf("csrf tokens differ: first=%q second=%q third=%q", first, second, third)
	}
	res, out := doJSON(t, server.Client(), http.MethodPut, server.URL+"/api/admin/settings", map[string]string{"message_retention_days": "31"}, cookie, first)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("第一个标签页的 CSRF Token 已失效: status=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestPagesAssetsAndSecurityHeaders(t *testing.T) {
	server, _ := testServer(t)
	for _, path := range []string{"/", "/login", "/assets/ui.css", "/assets/components.css", "/assets/pages.css", "/assets/components.js", "/assets/app.js", "/assets/connections.js", "/assets/mailboxes.js", "/assets/automations.js", "/assets/workspace.css", "/assets/logo.svg"} {
		res, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("%s status=%d", path, res.StatusCode)
		}
		if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatalf("missing strict CSP for %s", path)
		}
		if path == "/" {
			page := string(body)
			if !strings.Contains(page, `<div class="dialog-frame">`) {
				t.Fatal("dialog frame wrapper is missing")
			}
			if strings.Contains(page, `<form method="dialog"`) {
				t.Fatal("dialog frame must not be a form because dynamic dialogs contain their own forms")
			}
			if !strings.Contains(page, `<img src="/assets/logo.svg?v=1.1.0" alt="">`) {
				t.Fatal("application brand logo is missing")
			}
		}
		if path == "/" || path == "/login" {
			if !strings.Contains(string(body), `<link rel="icon" type="image/svg+xml" href="/assets/logo.svg?v=1.1.0">`) {
				t.Fatalf("favicon is missing from %s", path)
			}
		}
		if path == "/assets/app.js" {
			asset := string(body)

			if !strings.Contains(asset, `event.target.id==='mailbox-filter-form'`) {
				t.Fatal("mailbox filter submit handler is missing")
			}
			if !strings.Contains(asset, `function resetPageState(name)`) || !strings.Contains(asset, `resetPageState(currentRoute)`) {
				t.Fatal("route changes must reset transient page state")
			}
			if !strings.Contains(asset, `setButtonBusy(target,true)`) || !strings.Contains(asset, `finally{if(target.isConnected)setButtonBusy(target,false)}`) {
				t.Fatal("async action buttons must show and clear a busy state")
			}
			if !strings.Contains(asset, `setButtonBusy(event.submitter,true)`) {
				t.Fatal("form submit buttons must show a busy state")
			}
			if !strings.Contains(asset, `api.mailboxMessages(mailboxID,page,10)`) || !strings.Contains(asset, `size:'wide'`) {
				t.Fatal("mailbox inbox must use a wide dialog with 10 messages per page")
			}
			if !strings.Contains(asset, `>查看正文</button>`) || !strings.Contains(asset, `api.mailboxMessage(mailboxID,messageID)`) {
				t.Fatal("mailbox list and inbox must expose latest codes and message details")
			}
		}
		if path == "/assets/components.js" {
			asset := string(body)
			if !strings.Contains(asset, `spinner.dataset.dynamicSpinner = ''`) || !strings.Contains(asset, `button.dataset.busyWasDisabled`) {
				t.Fatal("button busy helper must create a spinner and preserve disabled state")
			}
		}
		if path == "/assets/components.css" {
			asset := string(body)
			if !strings.Contains(asset, `.icon-button[aria-busy=true]`) {
				t.Fatal("busy icon button spinner styles are missing")
			}
			if !strings.Contains(asset, `.dialog[data-size=wide]`) {
				t.Fatal("wide dialog styles are missing")
			}
		}
		if path == "/assets/pages.css" && !strings.Contains(string(body), `.inbox-pagination`) {
			t.Fatal("mailbox inbox pagination styles are missing")
		}
		if path == "/assets/logo.svg" {
			if res.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(string(body), `<svg xmlns="http://www.w3.org/2000/svg"`) {
				t.Fatal("brand logo must be served as SVG")
			}
		}
	}
	res, err := server.Client().Get(server.URL + "/api/obsolete-health")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("old API status=%d", res.StatusCode)
	}
	for _, path := range []string{"/healthz", "/readyz", "/api/openapi.json"} {
		response, requestErr := server.Client().Get(server.URL + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("%s status=%d body=%s", path, response.StatusCode, body)
		}
	}
	asset, requestErr := server.Client().Get(server.URL + "/assets/app.js?v=1.0.0")
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	etag := asset.Header.Get("ETag")
	asset.Body.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/assets/app.js?v=1.0.0", nil)
	request.Header.Set("If-None-Match", etag)
	asset, requestErr = server.Client().Do(request)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	asset.Body.Close()
	if etag == "" || asset.StatusCode != http.StatusNotModified {
		t.Fatalf("etag=%q status=%d", etag, asset.StatusCode)
	}
}

func TestRemoteIPOnlyTrustsConfiguredProxies(t *testing.T) {
	_, trusted, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trustedProxies: []*net.IPNet{trusted}}

	spoofed := httptest.NewRequest(http.MethodGet, "/", nil)
	spoofed.RemoteAddr = "203.0.113.20:1234"
	spoofed.Header.Set("X-Forwarded-For", "198.51.100.10")
	if got := s.remoteIP(spoofed); got != "203.0.113.20" {
		t.Fatalf("不可信来源伪造 XFF: %q", got)
	}

	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.RemoteAddr = "10.9.0.2:4321"
	forwarded.Header.Set("X-Forwarded-For", "198.51.100.10, 10.8.0.4")
	if got := s.remoteIP(forwarded); got != "198.51.100.10" {
		t.Fatalf("可信代理解析结果=%q", got)
	}
}

func TestOpenAPIPathsMatchRegisteredRoutes(t *testing.T) {
	data, err := web.FS.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	store, err := repository.Open(context.Background(), filepath.Join(t.TempDir(), "small-apple-mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := New(config.Config{}, store, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	methods := map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}
	for path, operations := range document.Paths {
		requestPath := strings.NewReplacer(
			"{account_id}", "acc_example", "{kind}", "imap", "{mailbox_id}", "mbx_example",
			"{job_id}", "job_example", "{schedule_id}", "sch_example", "{client_id}", "cli_example",
			"{activity_id}", "1", "{lease_id}", "lea_example",
		).Replace(path)
		for method := range operations {
			if !methods[method] {
				continue
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(strings.ToUpper(method), requestPath, nil)
			handler.ServeHTTP(recorder, request)
			if recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed {
				t.Errorf("OpenAPI 声明未注册: %s %s => %d", method, path, recorder.Code)
			}
		}
	}
}

func TestAdminAndLeaseContract(t *testing.T) {
	server, store := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/accounts", map[string]any{"apple_id": "owner@icloud.com", "display_name": "Owner", "note": ""}, cookie, csrf)
	if res.StatusCode != 201 {
		t.Fatalf("account=%d error=%+v", res.StatusCode, out.Error)
	}
	var account struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out.Data, &account)
	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes", map[string]any{"account_id": account.ID, "email": "alias@icloud.com", "label": "test", "note": "", "tags": []string{"openai"}}, cookie, csrf)
	if res.StatusCode != 201 {
		t.Fatalf("mailbox=%d error=%+v", res.StatusCode, out.Error)
	}
	var mailbox domain.Mailbox
	if err := json.Unmarshal(out.Data, &mailbox); err != nil || mailbox.ID == "" {
		t.Fatalf("mailbox=%+v err=%v", mailbox, err)
	}
	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/api-clients", map[string]any{"name": "automation", "scopes": []string{"leases:read", "leases:write", "codes:read"}}, cookie, csrf)
	if res.StatusCode != 201 {
		t.Fatalf("client=%d error=%+v", res.StatusCode, out.Error)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(out.Data, &created)
	if created.APIKey == "" {
		t.Fatal("empty api key")
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/leases", strings.NewReader(`{"project":"test","ttl_seconds":1800,"tags":["openai"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+created.APIKey)
	req.Header.Set("Idempotency-Key", "test-idempotency")
	leaseRes, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var leaseOut envelope
	_ = json.NewDecoder(leaseRes.Body).Decode(&leaseOut)
	leaseRes.Body.Close()
	if leaseRes.StatusCode != 201 {
		t.Fatalf("lease=%d error=%+v", leaseRes.StatusCode, leaseOut.Error)
	}
	var lease struct {
		ID    string `json:"id"`
		Token string `json:"lease_token"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(leaseOut.Data, &lease)
	if lease.ID == "" || lease.Token == "" || lease.Email != "alias@icloud.com" {
		t.Fatalf("lease=%+v", lease)
	}
	var active int
	if err = store.DB().QueryRow("SELECT COUNT(*) FROM leases WHERE status='active'").Scan(&active); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	if _, err = store.SaveMessages(context.Background(), []repository.MessageInput{{MailboxID: mailbox.ID, RemoteID: "icloud:INBOX:local-hit", Sender: "OpenAI", Subject: "Your code", Code: "123456", ReceivedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	codeURL := server.URL + "/api/leases/" + lease.ID + "/code?after=" + url.QueryEscape(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
	codeReq, _ := http.NewRequest(http.MethodGet, codeURL, nil)
	codeReq.Header.Set("Authorization", "Bearer "+created.APIKey)
	codeReq.Header.Set("X-Lease-Token", lease.Token)
	codeRes, err := server.Client().Do(codeReq)
	if err != nil {
		t.Fatal(err)
	}
	codeRes.Body.Close()
	if codeRes.StatusCode != http.StatusOK {
		t.Fatalf("code status=%d", codeRes.StatusCode)
	}
	var syncJobs int
	if err = store.DB().QueryRow("SELECT COUNT(*) FROM jobs WHERE type='mailbox.message_sync'").Scan(&syncJobs); err != nil || syncJobs != 0 {
		t.Fatalf("本地命中不应创建同步任务: jobs=%d err=%v", syncJobs, err)
	}
}

func TestMutationRequiresCSRF(t *testing.T) {
	server, _ := testServer(t)
	cookie, _ := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/accounts", map[string]any{"apple_id": "owner@icloud.com"}, cookie, "")
	if res.StatusCode != 403 || out.Error == nil || out.Error.Code != "csrf_invalid" {
		t.Fatalf("status=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestAccountDeleteRequiresMatchingPreviewToken(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/accounts", map[string]any{"apple_id": "delete-me@icloud.com", "display_name": "Delete Me", "note": ""}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("account=%d error=%+v", res.StatusCode, out.Error)
	}
	var account struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Data, &account); err != nil || account.ID == "" {
		t.Fatalf("account=%+v err=%v", account, err)
	}

	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/accounts/"+account.ID+"/delete-preview", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview=%d error=%+v", res.StatusCode, out.Error)
	}
	var preview struct {
		ConfirmationToken string         `json:"confirmation_token"`
		Impact            map[string]int `json:"impact"`
	}
	if err := json.Unmarshal(out.Data, &preview); err != nil || preview.ConfirmationToken == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if preview.Impact["connections"] != 0 || preview.Impact["mailboxes"] != 0 {
		t.Fatalf("impact=%+v", preview.Impact)
	}

	res, out = doJSON(t, server.Client(), "DELETE", server.URL+"/api/admin/accounts/"+account.ID, map[string]any{"confirmation_token": "wrong-token"}, cookie, csrf)
	if res.StatusCode != http.StatusConflict || out.Error == nil || out.Error.Code != "account_delete_preview_expired" {
		t.Fatalf("wrong token status=%d error=%+v", res.StatusCode, out.Error)
	}

	res, _ = doJSON(t, server.Client(), "DELETE", server.URL+"/api/admin/accounts/"+account.ID, map[string]any{"confirmation_token": preview.ConfirmationToken}, cookie, csrf)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", res.StatusCode)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/accounts/"+account.ID, nil, cookie, "")
	if res.StatusCode != http.StatusNotFound || out.Error == nil || out.Error.Code != "account_not_found" {
		t.Fatalf("deleted account status=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestMailboxValidationAndEditableFields(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/accounts", map[string]any{"apple_id": "mailbox-owner@icloud.com", "display_name": "Mailbox Owner", "note": ""}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("account=%d error=%+v", res.StatusCode, out.Error)
	}
	var account struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out.Data, &account)

	for name, body := range map[string]map[string]any{
		"empty":   {"account_id": account.ID, "email": ""},
		"invalid": {"account_id": account.ID, "email": "not-an-email"},
	} {
		t.Run(name, func(t *testing.T) {
			response, envelope := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes", body, cookie, csrf)
			if response.StatusCode != http.StatusBadRequest || envelope.Error == nil || envelope.Error.Code != "invalid_email" {
				t.Fatalf("status=%d error=%+v", response.StatusCode, envelope.Error)
			}
		})
	}

	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes", map[string]any{"account_id": account.ID, "email": "EDIT-ME@icloud.com", "label": "old", "note": "old note", "tags": []string{"one"}}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("mailbox=%d error=%+v", res.StatusCode, out.Error)
	}
	var mailbox struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out.Data, &mailbox)

	res, out = doJSON(t, server.Client(), "PATCH", server.URL+"/api/admin/mailboxes/"+mailbox.ID, map[string]any{"label": "", "note": "", "tags": []string{}, "local_enabled": false}, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch=%d error=%+v", res.StatusCode, out.Error)
	}
	var updated struct {
		Email        string   `json:"email"`
		Label        string   `json:"label"`
		Note         string   `json:"note"`
		Tags         []string `json:"tags"`
		Status       string   `json:"status"`
		LocalEnabled bool     `json:"local_enabled"`
	}
	_ = json.Unmarshal(out.Data, &updated)
	if updated.Email != "edit-me@icloud.com" || updated.Label != "" || updated.Note != "" || len(updated.Tags) != 0 || updated.LocalEnabled || updated.Status != "disabled" {
		t.Fatalf("updated=%+v", updated)
	}
}

func TestScheduleTypeAllowlist(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/schedules", map[string]any{"name": "bad", "type": "unsupported.task", "payload": map[string]any{}, "interval_seconds": 3600}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_schedule_type" {
		t.Fatalf("invalid schedule status=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/schedules", map[string]any{"name": "连接检查", "type": "account.connection.check", "payload": map[string]any{}, "interval_seconds": 3600}, cookie, csrf)
	if res.StatusCode != http.StatusCreated || out.Error != nil {
		t.Fatalf("valid schedule status=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestScheduleCRUDAndRun(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)

	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/schedules", map[string]any{
		"name":             "每小时连接检测",
		"type":             "account.connection.check",
		"payload":          map[string]any{"account_ids": []string{"acc_demo"}},
		"enabled":          false,
		"interval_seconds": 3600,
	}, cookie, csrf)
	if res.StatusCode != http.StatusCreated || out.Error != nil {
		t.Fatalf("create=%d error=%+v", res.StatusCode, out.Error)
	}
	var created struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Enabled  bool   `json:"enabled"`
		Interval int    `json:"interval_seconds"`
	}
	_ = json.Unmarshal(out.Data, &created)
	if created.ID == "" || created.Enabled || created.Interval != 3600 {
		t.Fatalf("created=%+v", created)
	}

	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/schedules/"+created.ID, nil, cookie, "")
	if res.StatusCode != http.StatusOK || out.Error != nil {
		t.Fatalf("get=%d error=%+v", res.StatusCode, out.Error)
	}

	res, out = doJSON(t, server.Client(), "PATCH", server.URL+"/api/admin/schedules/"+created.ID, map[string]any{
		"name":             "每日数据库检查",
		"type":             "database.integrity_check",
		"payload":          map[string]any{},
		"enabled":          true,
		"interval_seconds": 86400,
	}, cookie, csrf)
	if res.StatusCode != http.StatusOK || out.Error != nil {
		t.Fatalf("update=%d error=%+v", res.StatusCode, out.Error)
	}
	var updated struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Enabled  bool   `json:"enabled"`
		Interval int    `json:"interval_seconds"`
	}
	_ = json.Unmarshal(out.Data, &updated)
	if updated.Name != "每日数据库检查" || updated.Type != "database.integrity_check" || !updated.Enabled || updated.Interval != 86400 {
		t.Fatalf("updated=%+v", updated)
	}

	res, out = doJSON(t, server.Client(), "PATCH", server.URL+"/api/admin/schedules/"+created.ID, map[string]any{"enabled": false}, cookie, csrf)
	if res.StatusCode != http.StatusOK || out.Error != nil {
		t.Fatalf("partial update=%d error=%+v", res.StatusCode, out.Error)
	}

	res, out = doJSON(t, server.Client(), "PATCH", server.URL+"/api/admin/schedules/"+created.ID, map[string]any{"type": "unsupported.task"}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_schedule_type" {
		t.Fatalf("invalid update=%d error=%+v", res.StatusCode, out.Error)
	}

	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/schedules/"+created.ID+"/run", map[string]any{}, cookie, csrf)
	if res.StatusCode != http.StatusAccepted || out.Error != nil {
		t.Fatalf("run=%d error=%+v", res.StatusCode, out.Error)
	}
	var job struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(out.Data, &job)
	if job.Type != "database.integrity_check" {
		t.Fatalf("job=%+v", job)
	}

	res, out = doJSON(t, server.Client(), "DELETE", server.URL+"/api/admin/schedules/"+created.ID, nil, cookie, csrf)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/schedules/"+created.ID, nil, cookie, "")
	if res.StatusCode != http.StatusNotFound || out.Error == nil || out.Error.Code != "schedule_not_found" {
		t.Fatalf("get deleted=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestJobsArePaginatedWithinTwentyFourHours(t *testing.T) {
	server, store := testServer(t)
	cookie, _ := login(t, server)
	for i := 0; i < 25; i++ {
		if _, err := store.CreateJob(context.Background(), "database.integrity_check", "{}", 0); err != nil {
			t.Fatal(err)
		}
	}
	res, out := doJSON(t, server.Client(), "GET", server.URL+"/api/admin/jobs?limit=20", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("jobs=%d error=%+v", res.StatusCode, out.Error)
	}
	var first struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"next_cursor"`
		Retention  int               `json:"retention_hours"`
	}
	_ = json.Unmarshal(out.Data, &first)
	if len(first.Items) != 20 || first.NextCursor == "" || first.Retention != 24 {
		t.Fatalf("first=%+v", first)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/jobs?limit=20&cursor="+url.QueryEscape(first.NextCursor), nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("next jobs=%d error=%+v", res.StatusCode, out.Error)
	}
	var second struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"next_cursor"`
	}
	_ = json.Unmarshal(out.Data, &second)
	if len(second.Items) != 5 || second.NextCursor != "" {
		t.Fatalf("second=%+v", second)
	}
}

func TestMailboxAPICopyAndCodeLookup(t *testing.T) {
	server, store := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), "POST", server.URL+"/api/admin/accounts", map[string]any{"apple_id": "mailbox-api@icloud.com"}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("account=%d error=%+v", res.StatusCode, out.Error)
	}
	var account struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out.Data, &account)
	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes", map[string]any{"account_id": account.ID, "email": "api-copy@icloud.com"}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("mailbox=%d error=%+v", res.StatusCode, out.Error)
	}
	var mailbox struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out.Data, &mailbox)
	if _, err := store.SaveMessages(context.Background(), []repository.MessageInput{{MailboxID: mailbox.ID, RemoteID: "api-message", Sender: "security@example.com", Subject: "Your code is 482913", Code: "482913", MatchRule: "six-digit", Confidence: 0.97, Body: "Your verification code is 482913.", ReceivedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	regular := make([]repository.MessageInput, 0, 11)
	for i := 0; i < 11; i++ {
		regular = append(regular, repository.MessageInput{MailboxID: mailbox.ID, RemoteID: fmt.Sprintf("regular-message-%02d", i), Sender: "newsletter@example.com", Subject: fmt.Sprintf("Regular message %02d", i), ReceivedAt: time.Now().UTC().Add(-time.Duration(i+1) * time.Minute)})
	}
	if saved, err := store.SaveMessages(context.Background(), regular); err != nil || saved != 11 {
		t.Fatalf("saved regular messages=%d err=%v", saved, err)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/messages?page=1&page_size=10", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("message page 1=%d error=%+v", res.StatusCode, out.Error)
	}
	var messagePage struct {
		Items    []domain.Message `json:"items"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Pages    int              `json:"pages"`
	}
	if err := json.Unmarshal(out.Data, &messagePage); err != nil || len(messagePage.Items) != 10 || messagePage.Total != 12 || messagePage.Page != 1 || messagePage.PageSize != 10 || messagePage.Pages != 2 {
		t.Fatalf("message page 1=%+v err=%v", messagePage, err)
	}
	if messagePage.Items[0].Code != "482913" || messagePage.Items[0].Body != "" {
		t.Fatalf("列表应返回最新验证码但不携带正文: %+v", messagePage.Items[0])
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/messages/"+messagePage.Items[0].ID, nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("message detail=%d error=%+v", res.StatusCode, out.Error)
	}
	var detail domain.Message
	if err := json.Unmarshal(out.Data, &detail); err != nil || detail.Body != "Your verification code is 482913." {
		t.Fatalf("message detail=%+v err=%v", detail, err)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/messages/not-found", nil, cookie, "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing message=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), "GET", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/messages?page=2&page_size=10", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("message page 2=%d error=%+v", res.StatusCode, out.Error)
	}
	messagePage = struct {
		Items    []domain.Message `json:"items"`
		Total    int              `json:"total"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Pages    int              `json:"pages"`
	}{}
	if err := json.Unmarshal(out.Data, &messagePage); err != nil || len(messagePage.Items) != 2 || messagePage.Total != 12 || messagePage.Page != 2 {
		t.Fatalf("message page 2=%+v err=%v", messagePage, err)
	}

	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/api-credential", nil, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("credential=%d error=%+v", res.StatusCode, out.Error)
	}
	var first struct {
		APIURL string `json:"api_url"`
	}
	_ = json.Unmarshal(out.Data, &first)
	parsed, err := url.Parse(first.APIURL)
	if err != nil || parsed.Query().Get("key") == "" {
		t.Fatalf("api url=%q err=%v", first.APIURL, err)
	}
	res, out = doJSON(t, server.Client(), "GET", first.APIURL, nil, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("code=%d error=%+v", res.StatusCode, out.Error)
	}
	var code struct {
		Code  string `json:"code"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(out.Data, &code)
	if code.Code != "482913" || code.Email != "api-copy@icloud.com" {
		t.Fatalf("code=%+v", code)
	}

	res, out = doJSON(t, server.Client(), "POST", server.URL+"/api/admin/mailboxes/"+mailbox.ID+"/api-credential", nil, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("rotate=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), "GET", first.APIURL, nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized || out.Error == nil || out.Error.Code != "mailbox_api_invalid" {
		t.Fatalf("old credential=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestActivityCRUDAPI(t *testing.T) {
	server, store := testServer(t)
	cookie, csrf := login(t, server)
	if err := store.CreateActivity(context.Background(), domain.Activity{Source: "system", Type: "system.test", Result: "success", Detail: "系统记录"}); err != nil {
		t.Fatal(err)
	}
	res, out := doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/activities", map[string]any{"type": "manual.note", "result": "success", "detail": "手工记录"}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/activities?page=1&page_size=20", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list=%d error=%+v", res.StatusCode, out.Error)
	}
	var page struct {
		Items []domain.Activity `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(out.Data, &page); err != nil || page.Total < 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var system domain.Activity
	for _, item := range page.Items {
		if item.Type == "system.test" {
			system = item
		}
	}
	if system.ID == 0 || system.Source != "system" {
		t.Fatalf("system=%+v", system)
	}
	res, out = doJSON(t, server.Client(), http.MethodPatch, server.URL+"/api/admin/activities/"+strconv.FormatInt(system.ID, 10), map[string]any{"type": "system.edited", "result": "failed", "detail": "允许编辑"}, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("update=%d error=%+v", res.StatusCode, out.Error)
	}
	res, _ = doJSON(t, server.Client(), http.MethodDelete, server.URL+"/api/admin/activities/"+strconv.FormatInt(system.ID, 10), nil, cookie, csrf)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete=%d", res.StatusCode)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/activities", map[string]any{"type": "", "result": "unknown"}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil {
		t.Fatalf("invalid=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestAPIClientSettingsPasswordAndLogout(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/dashboard", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/api-clients", map[string]any{"name": "client-lifecycle", "scopes": []string{"leases:read"}}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create client=%d error=%+v", res.StatusCode, out.Error)
	}
	var created struct {
		Client domain.APIClient `json:"client"`
		APIKey string           `json:"api_key"`
	}
	if err := json.Unmarshal(out.Data, &created); err != nil || created.Client.ID == "" || !strings.HasPrefix(created.APIKey, "sam_live_") {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/api-clients", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list clients=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPatch, server.URL+"/api/admin/api-clients/"+created.Client.ID, map[string]any{"status": "disabled"}, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/api-clients/"+created.Client.ID+"/rotate", nil, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("rotate=%d error=%+v", res.StatusCode, out.Error)
	}
	res, _ = doJSON(t, server.Client(), http.MethodDelete, server.URL+"/api/admin/api-clients/"+created.Client.ID, nil, cookie, csrf)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete client=%d", res.StatusCode)
	}

	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/settings", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("settings=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, server.URL+"/api/admin/settings", map[string]string{"message_retention_days": "45"}, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("save settings=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, server.URL+"/api/admin/settings", map[string]string{"worker_concurrency": "5"}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "unknown_setting" {
		t.Fatalf("unknown setting=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, server.URL+"/api/admin/password", map[string]string{"current_password": "wrong", "new_password": "another correct password"}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_current_password" {
		t.Fatalf("wrong password=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, server.URL+"/api/admin/password", map[string]string{"current_password": "correct horse battery staple", "new_password": "another correct password"}, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("change password=%d error=%+v", res.StatusCode, out.Error)
	}
	res, _ = doJSON(t, server.Client(), http.MethodDelete, server.URL+"/api/admin/session", nil, cookie, csrf)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout=%d", res.StatusCode)
	}
	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/session", nil, cookie, "")
	if res.StatusCode != http.StatusUnauthorized || out.Error == nil {
		t.Fatalf("session after logout=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestJobDetailsCancelEventsAndClear(t *testing.T) {
	server, store := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/jobs", map[string]any{"type": "database.integrity_check", "payload": map[string]any{}, "priority": 3}, cookie, csrf)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("create=%d error=%+v", res.StatusCode, out.Error)
	}
	var job domain.Job
	if err := json.Unmarshal(out.Data, &job); err != nil || job.ID == "" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if _, err := store.AddJobEvent(context.Background(), job.ID, "queued", "info", "排队", `{}`); err != nil {
		t.Fatal(err)
	}
	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/jobs/"+job.ID, nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("detail=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodGet, server.URL+"/api/admin/jobs/"+job.ID+"/events", nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/jobs/"+job.ID+"/cancel", nil, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cancel=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/jobs", map[string]any{"type": "unknown.task", "payload": map[string]any{}}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_job_type" {
		t.Fatalf("invalid=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodDelete, server.URL+"/api/admin/jobs", nil, cookie, csrf)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("clear=%d error=%+v", res.StatusCode, out.Error)
	}
}

func TestConnectionAndSyncValidationPaths(t *testing.T) {
	server, _ := testServer(t)
	cookie, csrf := login(t, server)
	res, out := doJSON(t, server.Client(), http.MethodPost, server.URL+"/api/admin/accounts", map[string]any{"apple_id": "connection@icloud.com"}, cookie, csrf)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("account=%d error=%+v", res.StatusCode, out.Error)
	}
	var account domain.Account
	_ = json.Unmarshal(out.Data, &account)
	base := server.URL + "/api/admin/accounts/" + account.ID + "/connections"
	res, out = doJSON(t, server.Client(), http.MethodGet, base, nil, cookie, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("connections=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, base+"/imap", map[string]any{}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "imap_credentials_required" {
		t.Fatalf("imap=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPut, base+"/invalid", map[string]any{}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_connection_kind" {
		t.Fatalf("invalid kind=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, base+"/imap/verify", map[string]any{}, cookie, csrf)
	if res.StatusCode != http.StatusBadRequest || out.Error == nil || out.Error.Code != "invalid_connection_kind" {
		t.Fatalf("verify=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodPost, base+"/imap/check", nil, cookie, csrf)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("check=%d error=%+v", res.StatusCode, out.Error)
	}
	res, out = doJSON(t, server.Client(), http.MethodDelete, base+"/imap", nil, cookie, csrf)
	if res.StatusCode != http.StatusNotFound || out.Error == nil {
		t.Fatalf("delete missing=%d error=%+v", res.StatusCode, out.Error)
	}
	for _, path := range []string{"/api/admin/mailbox-sync-previews", "/api/admin/mailbox-clean-previews"} {
		res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+path, map[string]any{"account_ids": []string{account.ID}}, cookie, csrf)
		if res.StatusCode != http.StatusBadRequest || out.Error == nil {
			t.Fatalf("preview %s=%d error=%+v", path, res.StatusCode, out.Error)
		}
	}
	for _, path := range []string{"/api/admin/mailbox-sync-jobs", "/api/admin/mailbox-clean-jobs"} {
		res, out = doJSON(t, server.Client(), http.MethodPost, server.URL+path, map[string]any{"confirmation_token": "missing"}, cookie, csrf)
		if res.StatusCode != http.StatusConflict || out.Error == nil {
			t.Fatalf("confirm %s=%d error=%+v", path, res.StatusCode, out.Error)
		}
	}
}
