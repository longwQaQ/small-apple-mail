package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const validSessionResponse = `{"dsInfo":{"dsid":"test","appleId":"test@example.com","isHideMyEmailFeatureAvailable":true},"webservices":{"premiummailsettings":{"url":"https://test.icloud.com"}}}`

func TestWebSessionRefreshPersistsCookiesAndTokens(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "验证续期", true: "凭据恢复"}[recovery], func(t *testing.T) {
			validations, logins := 0, 0
			client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
				res := &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(validSessionResponse))}
				if strings.HasSuffix(r.URL.Path, "/validate") {
					validations++
					if recovery && validations == 1 {
						res.StatusCode = 401
					} else {
						res.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-USER=rotated; Domain=.icloud.com; Path=/; Max-Age=3600")
					}
				} else if strings.HasSuffix(r.URL.Path, "/accountLogin") {
					logins++
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["dsWebAuthToken"] != "saved-token" || body["trustToken"] != "saved-trust" {
						t.Fatal("未使用保存的 Apple 会话凭据")
					}
					res.Header.Set("X-Apple-Session-Token", "rotated-token")
				} else {
					t.Fatalf("非预期请求路径：%s", r.URL.Path)
				}
				return res, nil
			})}}
			session := ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "saved-token", TrustToken: "saved-trust"}}
			updated, err := client.RefreshICloudSession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.Cookies) != 1 || updated.Cookies[0].Value != "rotated" || updated.Cookies[0].Expires < float64(time.Now().Add(59*time.Minute).Unix()) {
				t.Fatal("新 Cookie 及有效期未保留")
			}
			if recovery && (logins != 1 || validations != 2 || updated.WebAuth.SessionToken != "rotated-token") {
				t.Fatal("恢复链或轮换 token 未持久化")
			}
			if !recovery && logins != 0 {
				t.Fatal("有效会话不应额外登录")
			}
		})
	}
}

func TestWebSessionRecoveryStopsAndClassifiesFailures(t *testing.T) {
	for _, status := range []int{401, 403, 421, 429, 450, 503} {
		calls := 0
		client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"7200"}}, Request: r, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}}
		_, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "test"}})
		failure := ClassifySessionError(err)
		wantLogin := status == 401 || status == 421 || status == 450
		wantCalls := 1
		if wantLogin {
			wantCalls = 2
		}
		if err == nil || calls != wantCalls || failure.RequiresLogin != wantLogin || failure.RateLimited != (status == 429) || failure.RetryAfter != 2*time.Hour {
			t.Fatalf("HTTP %d 的恢复次数或错误分类不正确：calls=%d failure=%+v", status, calls, failure)
		}
	}
}

func TestValidateRejectsUnfinishedTwoFactor(t *testing.T) {
	client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(`{"hsaChallengeRequired":true}`))}, nil
	})}}
	_, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com"})
	if !ClassifySessionError(err).RequiresLogin {
		t.Fatal("未完成 2FA 不应识别为正常")
	}
}

func TestCookieMaxAgeOverridesExpires(t *testing.T) {
	u, _ := url.Parse("https://setup.icloud.com/setup/ws/1/validate")
	cookies := []SessionCookie{{Name: "test", Value: "old", Domain: "setup.icloud.com", Path: "/"}}
	mergeSessionCookies(&cookies, u, []*http.Cookie{{Name: "test", Value: "new", Path: "/", MaxAge: 3600, Expires: time.Now().Add(-time.Hour)}})
	if len(cookies) != 1 || cookies[0].Expires < float64(time.Now().Unix()) {
		t.Fatal("Max-Age 应优先于 Expires")
	}
	mergeSessionCookies(&cookies, u, []*http.Cookie{{Name: "test", Value: "deleted", Path: "/", MaxAge: -1}})
	if len(cookies) != 0 {
		t.Fatal("Max-Age 删除指令未生效")
	}
}

func TestWebSessionRegionRedirectIsBounded(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		calls := 0
		client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls > 2 {
				t.Fatal("地区切换无限重试")
			}
			status, body := 200, validSessionResponse
			if calls == 1 || repeated {
				status, body = 421, `{"accountCountryCode":"CN"}`
			}
			if calls == 2 && r.URL.Host != "setup.icloud.com.cn" {
				t.Fatal("未使用白名单中国区端点")
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		updated, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com"})
		if calls != 2 || (!repeated && (err != nil || updated.Host != "www.icloud.com.cn")) || (repeated && !ClassifySessionError(err).RequiresLogin) {
			t.Fatalf("地区切换结果不正确：calls=%d err=%v", calls, err)
		}
	}
}

func TestMaintenanceTokenRecoveryIsOneExchange(t *testing.T) {
	calls := 0
	client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/accountLogin") {
			calls++
			return nil, io.EOF
		}
		return &http.Response{StatusCode: 401, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	_, _ = client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "synthetic"}})
	if calls != 1 {
		t.Fatalf("accountLogin sent %d times after a single failed validation", calls)
	}
}

func TestMaintenanceRecoveryRetainsRotatedCredentialsOnFinal503(t *testing.T) {
	validations := 0
	client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		res := &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}
		if strings.HasSuffix(r.URL.Path, "/accountLogin") {
			res.Header.Set("X-Apple-Session-Token", "rotated-token")
			res.Header.Set("Set-Cookie", "X-APPLE-WEBAUTH-USER=rotated; Domain=.icloud.com; Path=/; Max-Age=3600")
			return res, nil
		}
		validations++
		if validations == 1 {
			res.StatusCode = 401
		} else {
			res.StatusCode = 503
		}
		return res, nil
	})}}
	got, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "old-token"}})
	if err == nil {
		t.Fatal("expected retryable 503")
	}
	if got.WebAuth.SessionToken != "rotated-token" || len(got.Cookies) != 1 {
		t.Fatal("successful accountLogin response credentials discarded after final validate 503")
	}
}
func TestMaintenanceValidationRejectsIdentityMismatch(t *testing.T) {
	client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(validSessionResponse))}, nil
	})}}
	got, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com", DSID: "original-account", AppleID: "original@example.com"})
	if err == nil || got.DSID != "original-account" {
		t.Fatalf("identity mismatch accepted err=%v dsid=%s appleID=%s", err, got.DSID, got.AppleID)
	}
}

func TestValidateRotatesHeadersAndRejectsUnsafeServices(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		body := validSessionResponse
		if unsafe {
			body = strings.ReplaceAll(body, "https://test.icloud.com", "https://example.invalid")
		}
		client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"X-Apple-Session-Token": []string{"rotated"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		original := ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "original", TrustToken: "retained"}}
		got, err := client.RefreshICloudSession(context.Background(), original)
		if unsafe {
			if err == nil || got.WebAuth.SessionToken != "original" {
				t.Fatal("异常服务地址不能覆盖已保存会话")
			}
		} else if err != nil || got.WebAuth.SessionToken != "rotated" || got.WebAuth.TrustToken != "retained" {
			t.Fatal("验证响应未保留轮换 token 或原有 trust token", err)
		}
	}
}

func TestSessionRetryAfterHTTPDate(t *testing.T) {
	until := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	err := withHTTPDetails(errCode("test", "暂时不可用", true), &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{until.Format(http.TimeFormat)}}})
	failure := ClassifySessionError(err)
	if !failure.Temporary || failure.RetryAfter < 119*time.Minute || failure.RetryAfter > 2*time.Hour {
		t.Fatal("未按 HTTP 日期解析等待时间")
	}
}

func TestAppleMaintenanceStopsAtRateLimit(t *testing.T) {
	calls, manageCalls := 0, 0
	client := &ICloudClient{client: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		response := &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"timeOutInterval":10,"apiKey":"synthetic"}`))}
		if r.URL.Path == "/account/manage" {
			manageCalls++
			if manageCalls == 1 {
				response.StatusCode = 429
				response.Header.Set("Retry-After", "7200")
			}
		}
		return response, nil
	})}}
	_, err := client.KeepAliveAppleAccountManageState(context.Background(), LoginState{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic"})
	failure := ClassifySessionError(err)
	if calls != 2 || !failure.RateLimited || failure.RetryAfter != 2*time.Hour {
		t.Fatalf("429 ignored: calls=%d manageCalls=%d err=%v failure=%+v", calls, manageCalls, err, failure)
	}
}
func TestApplePortalKeepsRetryAfter(t *testing.T) {
	client := &ICloudClient{client: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Request: r, Header: http.Header{"Retry-After": []string{"7200"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	state := LoginState{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic"}
	err := client.warmAppleAccountPortal(context.Background(), &state)
	failure := ClassifySessionError(err)
	if !failure.RateLimited || failure.RetryAfter != 2*time.Hour {
		t.Fatalf("portal 429 metadata lost: %+v", failure)
	}
}

func TestAppleMaintenanceStopsAtEachFailedStage(t *testing.T) {
	for _, stage := range []struct {
		path            string
		occurrence, ttl int
	}{
		{"/account/manage/gs/ws/token", 1, 10},
		{"/account/manage", 1, 10},
		{"/account/manage/section/privacy", 1, 0},
		{"/bootstrap/portal", 1, 0},
		{"/account/manage/gs/ws/token", 2, 0},
		{"/account/manage/forwardemail", 1, 10},
		{"/v2/jslogs", 1, 10},
	} {
		for _, status := range []int{401, 429, 503} {
			failed, occurrences := false, 0
			client := &ICloudClient{client: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
				if failed {
					t.Fatalf("HTTP %d 后仍继续请求 %s", status, r.URL.Path)
				}
				res := &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"timeOutInterval":10,"apiKey":"synthetic"}`))}
				if stage.ttl == 0 {
					res.Body = io.NopCloser(strings.NewReader(`{"timeOutInterval":0,"apiKey":"synthetic"}`))
				}
				if r.URL.Path == stage.path {
					occurrences++
					if occurrences == stage.occurrence {
						failed = true
						res.StatusCode = status
						res.Header.Set("Retry-After", "7200")
					}
				}
				return res, nil
			})}}
			got, err := client.KeepAliveAppleAccountManageState(context.Background(), LoginState{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic"})
			if !failed || err == nil || got.Scnt == "" || ClassifySessionError(err).RetryAfter != 2*time.Hour {
				t.Fatalf("阶段 %s 没有保留失败状态或原会话：%v", stage.path, err)
			}
		}
	}
}

func TestAppleCreateDoesNotRepeatAfterAuthFailure(t *testing.T) {
	calls := 0
	client := &ICloudClient{client: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	session := ICloudSession{LoginStates: []LoginState{{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic", APIKey: "synthetic", LastCheckedAt: time.Now(), ManageExpiresAt: time.Now().Add(time.Hour)}}}
	_, _, err := client.CreatePrivacyMailboxWithAppleAccount(context.Background(), session, "", "test", "")
	if err == nil || calls != 1 {
		t.Fatal("创建失败不应在协议层自动恢复并重放", err, calls)
	}
}

func TestMaintenanceDoesNotFollowHTTPRedirect(t *testing.T) {
	calls := 0
	client := &AppleAuthClient{httpClient: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Request: r, Header: http.Header{"Location": []string{"https://example.invalid/receive"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	_, err := client.RefreshICloudSession(context.Background(), ICloudSession{Host: "www.icloud.com", WebAuth: &ICloudWebAuth{SessionToken: "synthetic"}})
	if err == nil || calls != 1 {
		t.Fatal("后台维护不应跟随未经验证的 Location", calls, err)
	}
}

func TestAppleCreateDoesNotRetryUnknownNetworkResult(t *testing.T) {
	calls := 0
	client := &ICloudClient{client: &http.Client{Transport: sessionTransport(func(r *http.Request) (*http.Response, error) { calls++; return nil, io.EOF })}}
	session := ICloudSession{LoginStates: []LoginState{{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "synthetic", APIKey: "synthetic", LastCheckedAt: time.Now(), ManageExpiresAt: time.Now().Add(time.Hour)}}}
	_, _, err := client.CreatePrivacyMailboxWithAppleAccount(context.Background(), session, "", "test", "")
	if err == nil || calls != 1 {
		t.Fatal("网络中断后不能重放结果未知的创建请求", err, calls)
	}
}

func TestAppleConnectionRefreshPersistsManagementCredentials(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Scnt": []string{"rotated"}, "Set-Cookie": []string{"session=updated; Domain=.apple.com; Path=/; Max-Age=3600"}}, Body: io.NopCloser(strings.NewReader(`{"timeOutInterval":15,"apiKey":"updated-key"}`))}, nil
	})
	before := ICloudSession{Host: "appleid.apple.com", LoginStates: []LoginState{{Kind: LoginStateAppleAccount, Host: "appleid.apple.com", Scnt: "original"}}}
	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	raw, expires, err := RefreshConnection(context.Background(), LoginStateAppleAccount, string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	var updated ICloudSession
	if err := json.Unmarshal([]byte(raw), &updated); err != nil {
		t.Fatal(err)
	}
	state, ok := appleAccountLoginState(updated)
	if !ok || calls != 4 || state.Scnt != "rotated" || state.APIKey != "updated-key" || len(state.Cookies) != 1 || state.Cookies[0].Value != "updated" || expires.Before(time.Now().Add(14*time.Minute)) || !state.LastCheckOK {
		t.Fatal("管理维护未返回完整刷新状态", calls)
	}
}
