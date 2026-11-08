package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
)

func connectionFixture(t *testing.T) (*ConnectionService, domain.Connection) {
	t.Helper()
	store, err := repository.Open(context.Background(), filepath.Join(t.TempDir(), "connections.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	account, err := store.CreateAccount(context.Background(), "test@example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.UpsertConnection(context.Background(), account.ID, domain.ConnectionICloudWeb, domain.ConnectionHealthy, "www.icloud.com", `{"host":"www.icloud.com","note":"original"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	return newConnectionService(store, events.New()), conn
}

func TestHealthCheckPersistsRefreshedSession(t *testing.T) {
	s, c := connectionFixture(t)
	s.refresh = func(context.Context, string, string) (string, time.Time, error) {
		return `{"note":"rotated"}`, time.Now().Add(time.Hour), nil
	}
	if err := s.check(context.Background(), c.AccountID, c.Kind, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
	if err != nil || got.SecretJSON != `{"note":"rotated"}` || got.Status != domain.ConnectionHealthy || got.NextCheckAt.IsZero() || got.ExpiresAt.IsZero() {
		t.Fatal("刷新后的会话或维护状态未保存", err)
	}
}

func TestMailboxListingPersistsRotatedCookiesAndHonorsCooldown(t *testing.T) {
	s, c := connectionFixture(t)
	ctx := context.Background()
	session := protocol.ICloudSession{Host: "www.icloud.com", DSID: "fixture", PremiumMailBaseURL: "https://test.icloud.com", Cookies: []protocol.SessionCookie{{Name: "test", Value: "original", Domain: ".icloud.com", Path: "/"}}}
	encoded, _ := json.Marshal(session)
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, c.Kind, domain.ConnectionHealthy, session.Host, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		want := "original"
		if calls > 1 {
			want = "rotated"
		}
		cookie, err := r.Cookie("test")
		if err != nil || cookie.Value != want {
			t.Fatalf("第 %d 次列表请求未使用最新凭据", calls)
		}
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Set-Cookie": {"test=rotated; Domain=.icloud.com; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{"hmeEmails":[]}}`))}, nil
	})
	for range 2 {
		if _, err := s.ListPrivacyMailboxes(ctx, c.AccountID); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.store.ConnectionSecret(ctx, c.AccountID, c.Kind)
	after := before
	after.Status = domain.ConnectionRateLimited
	after.NextCheckAt = time.Now().Add(time.Hour)
	if saved, err := s.store.SaveConnectionCheck(ctx, before, after); err != nil || !saved {
		t.Fatal("未能设置等待重试状态", err)
	}
	if _, err := s.ListPrivacyMailboxes(ctx, c.AccountID); err == nil || calls != 2 {
		t.Fatal("等待重试期间仍请求邮箱列表", err)
	}
}

func TestBackgroundResultCannotOverwriteNewLogin(t *testing.T) {
	for _, refreshErr := range []error{nil, errors.New("临时失败")} {
		s, c := connectionFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		s.refresh = func(context.Context, string, string) (string, time.Time, error) {
			close(entered)
			<-release
			return `{"note":"old-response"}`, time.Time{}, refreshErr
		}
		done := make(chan error, 1)
		go func() { done <- s.check(context.Background(), c.AccountID, c.Kind, false) }()
		<-entered
		if _, err := s.store.UpsertConnection(context.Background(), c.AccountID, c.Kind, domain.ConnectionChecking, c.Host, `{"note":"new-login"}`, "新登录"); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal("过期任务结果应被忽略", err)
		}
		got, _ := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
		if got.SecretJSON != `{"note":"new-login"}` || got.Status != domain.ConnectionChecking {
			t.Fatal("后台结果覆盖了新登录")
		}
	}
}

func TestChecksForSameConnectionUseLatestCredentials(t *testing.T) {
	s, c := connectionFixture(t)
	s.refresh = func(_ context.Context, _ string, raw string) (string, time.Time, error) {
		var session protocol.ICloudSession
		json.Unmarshal([]byte(raw), &session)
		session.Note += "x"
		encoded, _ := json.Marshal(session)
		return string(encoded), time.Time{}, nil
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.check(context.Background(), c.AccountID, c.Kind, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
	var session protocol.ICloudSession
	json.Unmarshal([]byte(got.SecretJSON), &session)
	if session.Note != "originalxx" {
		t.Fatal("并发检测未使用上一轮保存的新凭据")
	}
}

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaintenanceFailuresStopOrBackOff(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, status := range []int{401, 429, 503} {
		s, c := connectionFixture(t)
		calls := 0
		http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Retry-After": []string{"7200"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})
		if err := s.check(context.Background(), c.AccountID, c.Kind, true); err == nil {
			t.Fatal("应报告远端失败")
		}
		got, _ := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
		var retained protocol.ICloudSession
		json.Unmarshal([]byte(got.SecretJSON), &retained)
		if retained.Note != "original" {
			t.Fatal("失败时不得清除原凭据")
		}
		if status == 401 {
			if got.Status != domain.ConnectionExpired || !got.NextCheckAt.IsZero() {
				t.Fatal("鉴权失败应暂停并要求重新登录")
			}
		} else if got.NextCheckAt.Before(time.Now().Add(119 * time.Minute)) {
			t.Fatal("服务端 Retry-After 被本地重试提前截断")
		}
		if err := s.check(context.Background(), c.AccountID, c.Kind, true); err != nil || calls != 1 {
			t.Fatal("暂停或退避期间仍发起自动请求", err)
		}
	}
}

func TestConnectionMaintenanceTiming(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{domain.ConnectionAppleAccount, domain.ConnectionICloudWeb} {
		c := domain.Connection{ID: "test", Kind: kind}
		next := nextConnectionCheck(c, now.Add(2*time.Minute), now)
		if !next.Equal(now.Add(time.Minute)) {
			t.Fatal("应在凭据临期前维护")
		}
		next = nextConnectionCheck(c, now.Add(5*time.Second), now)
		if next.Before(now.Add(30 * time.Second)) {
			t.Fatal("临期状态不应触发忙循环")
		}
	}
	c := domain.Connection{Kind: domain.ConnectionICloudWeb}
	last := time.Duration(0)
	for range 10 {
		c = connectionCheckResult(c, "", time.Time{}, errors.New("超时"), now)
		delay := c.NextCheckAt.Sub(now)
		if delay < last || delay > time.Hour {
			t.Fatal("退避应递增且最多一小时")
		}
		last = delay
	}
}

func TestMaintenanceScheduledCheckHonorsPause(t *testing.T) {
	s, c := connectionFixture(t)
	if err := s.store.SetConnectionStatus(context.Background(), c.AccountID, c.Kind, domain.ConnectionExpired, "expired"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.refresh = func(context.Context, string, string) (string, time.Time, error) {
		calls++
		return c.SecretJSON, time.Time{}, nil
	}
	schedule, err := s.store.CreateSchedule(context.Background(), "account.connection.check", "legacy check", `{}`, true, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DB().Exec(`UPDATE schedules SET next_run_at='2000-01-01T00:00:00Z' WHERE id=?`, schedule.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.store.EnqueueDueSchedules(context.Background())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	if err := s.checkJob(context.Background(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("expired connection unexpectedly requested %d times from scheduled job payload=%s", calls, jobs[0].Payload)
	}
}
func TestMaintenanceBusinessHonorsCooldown(t *testing.T) {
	s, c := connectionFixture(t)
	after := c
	after.Status = domain.ConnectionRateLimited
	after.NextCheckAt = time.Now().Add(2 * time.Hour)
	if _, err := s.store.SaveConnectionCheck(context.Background(), c, after); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_ = s.useSession(context.Background(), c.AccountID, c.Kind, func(session protocol.ICloudSession) (protocol.ICloudSession, error) { calls++; return session, nil })
	if calls != 0 {
		t.Fatalf("business operation called during Retry-After cooldown: %d", calls)
	}
}
func TestMaintenanceBusiness503HonorsRetryAfter(t *testing.T) {
	s, c := connectionFixture(t)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Request: r, Header: http.Header{"Retry-After": []string{"7200"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_ = s.useSession(context.Background(), c.AccountID, c.Kind, func(session protocol.ICloudSession) (protocol.ICloudSession, error) {
		_, _, err := protocol.RefreshConnection(context.Background(), c.Kind, c.SecretJSON)
		return session, err
	})
	got, err := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextCheckAt.Before(time.Now().Add(119 * time.Minute)) {
		t.Fatalf("503 Retry-After not saved: status=%s next=%s", got.Status, got.NextCheckAt)
	}
}

func TestBusinessAuthRecoveryDoesNotReplayOperation(t *testing.T) {
	s, c := connectionFixture(t)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, _, authErr := protocol.RefreshConnection(context.Background(), c.Kind, c.SecretJSON)
	operations, refreshes := 0, 0
	s.refresh = func(context.Context, string, string) (string, time.Time, error) {
		refreshes++
		return `{"host":"www.icloud.com","note":"recovered"}`, time.Time{}, nil
	}
	err := s.useSession(context.Background(), c.AccountID, c.Kind, func(session protocol.ICloudSession) (protocol.ICloudSession, error) {
		operations++
		return session, authErr
	})
	if err == nil || !strings.Contains(err.Error(), "重新执行") || operations != 1 || refreshes != 1 {
		t.Fatal("授权恢复后不能自动重放业务操作", err)
	}
	got, err := s.store.ConnectionSecret(context.Background(), c.AccountID, c.Kind)
	var session protocol.ICloudSession
	if err != nil || json.Unmarshal([]byte(got.SecretJSON), &session) != nil || got.Status != domain.ConnectionHealthy || session.Note != "recovered" {
		t.Fatal("恢复后的会话未保存", err)
	}
	// 正常的下一次维护时间不能阻止用户执行普通操作。
	if err := s.useSession(context.Background(), c.AccountID, c.Kind, func(session protocol.ICloudSession) (protocol.ICloudSession, error) {
		operations++
		return session, nil
	}); err != nil || operations != 2 {
		t.Fatal("健康连接的维护时间不应限制业务", err)
	}
}

func TestAutomaticCreationPrefersICloudAndSkipsDisabledBackup(t *testing.T) {
	s, c := connectionFixture(t)
	ctx := context.Background()
	session := protocol.ICloudSession{Host: "www.icloud.com", DSID: "test", PremiumMailBaseURL: "https://test.icloud.com", IsICloudPlus: true, CanCreateHME: true, Cookies: []protocol.SessionCookie{{Name: "test", Value: "synthetic", Domain: ".icloud.com", Path: "/"}}}
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, c.Kind, domain.ConnectionHealthy, session.Host, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, domain.ConnectionAppleAccount, domain.ConnectionHealthy, "appleid.apple.com", `{}`, ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "test.icloud.com" {
			t.Fatalf("自动策略没有优先使用 iCloud：%s", r.URL.Host)
		}
		body := `{"success":true,"result":{"hme":"alias@example.com"}}`
		if strings.HasSuffix(r.URL.Path, "/reserve") {
			body = `{"success":true,"result":{"hme":{"hme":"alias@example.com","anonymousId":"id","isActive":true}}}`
		}
		return &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	mailbox, err := s.createRemoteMailbox(ctx, c.AccountID, "auto", "test", "")
	if err != nil || mailbox.Email != "alias@example.com" || calls != 2 {
		t.Fatal("iCloud 默认创建失败", mailbox, err, calls)
	}
	if err := s.store.SetConnectionStatus(ctx, c.AccountID, c.Kind, domain.ConnectionExpired, "测试失效"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.SetOptionalConnectionEnabled(ctx, c.AccountID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRemoteMailbox(ctx, c.AccountID, "auto", "test", ""); err == nil || calls != 2 {
		t.Fatal("不应使用已停用的备用通道", err, calls)
	}
	refreshes := 0
	s.refresh = func(context.Context, string, string) (string, time.Time, error) {
		refreshes++
		return "", time.Time{}, nil
	}
	if err := s.check(ctx, c.AccountID, domain.ConnectionAppleAccount, true); err != nil || refreshes != 0 {
		t.Fatal("已停用通道不应自动维护", err)
	}
}

func TestAutomaticCreationUsesHealthyBackupWhenICloudUnavailable(t *testing.T) {
	s, c := connectionFixture(t)
	ctx := context.Background()
	if err := s.store.SetConnectionStatus(ctx, c.AccountID, c.Kind, domain.ConnectionFailed, "测试失败"); err != nil {
		t.Fatal(err)
	}
	session := protocol.ICloudSession{Host: "appleid.apple.com", LoginStates: []protocol.LoginState{{Kind: protocol.LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic", APIKey: "synthetic", LastCheckedAt: time.Now(), ManageExpiresAt: time.Now().Add(time.Hour)}}}
	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, domain.ConnectionAppleAccount, domain.ConnectionHealthy, session.Host, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "appleid.apple.com" {
			t.Fatal("不应使用未通过检测的 iCloud 连接")
		}
		return &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"emailAddress":"backup@example.com","active":true}`))}, nil
	})
	mailbox, err := s.createRemoteMailbox(ctx, c.AccountID, "auto", "test", "")
	if err != nil || mailbox.Email != "backup@example.com" || calls != 2 {
		t.Fatal("基础创建通道不可用时未使用已启用的备用通道", mailbox, err, calls)
	}
}
