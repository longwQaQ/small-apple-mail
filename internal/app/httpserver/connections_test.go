package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"small-apple-mail/internal/app/domain"
)

func TestOptionalConnectionToggleRequiresAdminAndPreservesSecrets(t *testing.T) {
	server, store := testServer(t)
	ctx := context.Background()
	account, err := store.CreateAccount(ctx, "test@example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertConnection(ctx, account.ID, domain.ConnectionAppleAccount, domain.ConnectionHealthy, "", `{"token":"private-test-secret"}`, ""); err != nil {
		t.Fatal(err)
	}
	path := server.URL + "/api/admin/accounts/" + account.ID + "/connections/apple_account/enabled"
	cookie, csrf := login(t, server)
	cases := []struct {
		name, path string
		body       any
		auth, csrf bool
		status     int
	}{
		{"未登录", path, map[string]any{"enabled": false}, false, false, 401},
		{"缺少 CSRF", path, map[string]any{"enabled": false}, true, false, 403},
		{"缺少 enabled", path, map[string]any{}, true, true, 400},
		{"非备用通道", strings.Replace(path, "apple_account", "icloud_web", 1), map[string]any{"enabled": false}, true, true, 400},
		{"停用", path, map[string]any{"enabled": false}, true, true, 200},
		{"启用", path, map[string]any{"enabled": true}, true, true, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var auth *http.Cookie
			if tc.auth {
				auth = cookie
			}
			token := ""
			if tc.csrf {
				token = csrf
			}
			response, out := doJSON(t, server.Client(), "PATCH", tc.path, tc.body, auth, token)
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d error=%+v", response.StatusCode, tc.status, out.Error)
			}
			if strings.Contains(string(out.Data), "private-test-secret") || strings.Contains(string(out.Data), "secret_json") {
				t.Fatal("切换接口泄漏凭据")
			}
		})
	}
	got, err := store.ConnectionSecret(ctx, account.ID, domain.ConnectionAppleAccount)
	if err != nil || got.Status != domain.ConnectionChecking || !strings.Contains(got.SecretJSON, "private-test-secret") {
		t.Fatal("切换丢失凭据或未等待维护", err)
	}
}
