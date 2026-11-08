package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func appleAccountManageHostForICloudHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, "appleid.apple.com.cn") {
		return "appleid.apple.com.cn"
	}
	return "appleid.apple.com"
}

func appleAccountManageOriginForHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, "account.apple.com.cn") || strings.Contains(host, "appleid.apple.com.cn") {
		return "https://account.apple.com.cn"
	}
	return appleAccountManageOrigin
}

func appleAccountManageBaseForState(state LoginState) string {
	baseURL := strings.TrimSpace(appleAccountManageBaseURL)
	if baseURL == "" {
		baseURL = "https://appleid.apple.com"
	}
	if strings.TrimRight(baseURL, "/") != "https://appleid.apple.com" {
		return baseURL
	}
	host := strings.TrimSpace(state.Host)
	if host == "" {
		return baseURL
	}
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Trim(host, "/")
	if host == "" {
		return baseURL
	}
	return "https://" + host
}

func appleAccountPortalBaseForState(state LoginState) string {
	baseURL := strings.TrimSpace(appleAccountManageBaseURL)
	if baseURL != "" && strings.TrimRight(baseURL, "/") != "https://appleid.apple.com" {
		return baseURL
	}
	return strings.TrimRight(firstNonEmpty(state.Origin, appleAccountManageOriginForHost(state.Host), appleAccountManageOrigin), "/")
}

func appleAccountLoginState(session ICloudSession) (LoginState, bool) {
	for _, state := range session.LoginStates {
		if state.Kind != LoginStateAppleAccount {
			continue
		}
		if strings.TrimSpace(state.Scnt) == "" {
			continue
		}
		return state, true
	}
	return LoginState{}, false
}

func appleAccountManageReady(session ICloudSession) bool {
	state, ok := appleAccountLoginState(session)
	return ok && strings.TrimSpace(state.APIKey) != ""
}

func appleAccountManageNeedsCreateRefresh(loginState LoginState, now time.Time) bool {
	return !appleAccountManageRecentStateUsable(loginState, now)
}

func appleAccountManageRecentStateUsable(loginState LoginState, now time.Time) bool {
	if strings.TrimSpace(loginState.Scnt) == "" {
		return false
	}
	if strings.TrimSpace(loginState.APIKey) == "" {
		return false
	}
	if loginState.LastCheckedAt.IsZero() {
		return false
	}
	if loginState.ManageExpiresAt.IsZero() {
		return false
	}
	return now.Before(loginState.ManageExpiresAt.Add(-appleAccountManageRefreshSkew))
}

func appleAccountManageRecentlyOK(loginState LoginState, now time.Time) bool {
	return loginState.LastCheckOK && appleAccountManageRecentStateUsable(loginState, now)
}

func markAppleAccountManageOK(loginState *LoginState) {
	if loginState == nil {
		return
	}
	loginState.LastCheckedAt = time.Now()
	loginState.LastCheckOK = true
	loginState.LastStatusMessage = "备用创建通道登录态正常"
}

func markAppleAccountManageTokenTTL(loginState *LoginState, timeoutMinutes int, now time.Time) {
	if loginState == nil || timeoutMinutes <= 0 {
		return
	}
	loginState.ManageExpiresAt = now.Add(time.Duration(timeoutMinutes) * time.Minute)
}

func (c *ICloudClient) CheckAppleAccountManageSession(ctx context.Context, session ICloudSession) (ICloudSession, error) {
	loginState, ok := appleAccountLoginState(session)
	if !ok {
		return session, errCode("apple_account_session_missing", "未保存备用创建通道登录态，请先完成备用创建通道登录", true)
	}
	now := time.Now()
	if appleAccountManageRecentlyOK(loginState, now) && !appleAccountKeepAliveDue(loginState, now, appleAccountKeepAliveDefaultInterval) {
		return withAppleAccountLoginState(session, loginState), nil
	}
	release, err := acquireAppleAccountOperationGate(ctx, appleAccountOperationKey(session, loginState))
	if err != nil {
		return session, err
	}
	defer release()
	kept, err := c.keepAliveAppleAccountManageStateUnlocked(ctx, loginState)
	if err != nil {
		return session, err
	}
	return withAppleAccountLoginState(session, kept), nil
}

func withAppleAccountLoginState(session ICloudSession, next LoginState) ICloudSession {
	next.Kind = LoginStateAppleAccount
	for i, state := range session.LoginStates {
		if state.Kind == LoginStateAppleAccount {
			session.LoginStates[i] = next
			return session
		}
	}
	session.LoginStates = append(session.LoginStates, next)
	return session
}

func (c *ICloudClient) CreatePrivacyMailbox(ctx context.Context, session ICloudSession, label, note string) (ICloudRemoteMailbox, error) {
	mailbox, _, err := c.CreatePrivacyMailboxWithSession(ctx, session, label, note)
	return mailbox, err
}

func (c *ICloudClient) CreatePrivacyMailboxWithSession(ctx context.Context, session ICloudSession, label, note string) (ICloudRemoteMailbox, ICloudSession, error) {
	mailbox, err := c.createPrivacyMailbox(ctx, &session, label, note)
	return mailbox, session, err
}

func (c *ICloudClient) createPrivacyMailbox(ctx context.Context, session *ICloudSession, label, note string) (ICloudRemoteMailbox, error) {
	if strings.TrimSpace(session.PremiumMailBaseURL) == "" || strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return ICloudRemoteMailbox{}, errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	if !session.IsICloudPlus || !session.CanCreateHME {
		return ICloudRemoteMailbox{}, errCode("icloud_hme_unavailable", "当前登录态没有可创建隐私邮箱的 iCloud+ 权限", false)
	}

	generated, err := c.generate(ctx, session)
	if err != nil {
		return ICloudRemoteMailbox{}, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "UPI-" + time.Now().Format("0102-150405")
	}
	return c.reserve(ctx, session, generated, label, strings.TrimSpace(note))
}

func (c *ICloudClient) CreatePrivacyMailboxWithAppleAccount(ctx context.Context, session ICloudSession, fallbackAPIKey, label, note string) (ICloudRemoteMailbox, ICloudSession, error) {
	loginState, ok := appleAccountLoginState(session)
	if !ok {
		return ICloudRemoteMailbox{}, session, errCode("apple_account_session_missing", "当前登录态缺少 Apple Account 管理态，请重新协议登录", true)
	}
	release, err := acquireAppleAccountOperationGate(ctx, appleAccountOperationKey(session, loginState))
	if err != nil {
		return ICloudRemoteMailbox{}, session, err
	}
	defer release()

	fallbackAPIKey = strings.TrimSpace(fallbackAPIKey)
	if appleAccountManageNeedsCreateRefresh(loginState, time.Now()) {
		loginState, session, err = c.refreshAppleAccountManageStateForCreate(ctx, session, loginState, fallbackAPIKey)
		if err != nil {
			return ICloudRemoteMailbox{}, session, err
		}
	}

	return c.createPrivacyMailboxWithAppleAccountState(ctx, session, loginState, fallbackAPIKey, label, note)
}

func (c *ICloudClient) refreshAppleAccountManageStateForCreate(ctx context.Context, session ICloudSession, loginState LoginState, fallbackAPIKey string) (LoginState, ICloudSession, error) {
	refreshed, err := c.refreshAppleAccountManageStateUnlocked(ctx, loginState)
	loginState = refreshed
	session = withAppleAccountLoginState(session, loginState)
	if err != nil && (fallbackAPIKey == "" || !isCodedError(err, "apple_account_api_key_missing")) {
		return loginState, session, err
	}
	return loginState, session, nil
}

func (c *ICloudClient) createPrivacyMailboxWithAppleAccountState(ctx context.Context, session ICloudSession, loginState LoginState, fallbackAPIKey, label, note string) (ICloudRemoteMailbox, ICloudSession, error) {
	apiKey := strings.TrimSpace(firstNonEmpty(loginState.APIKey, fallbackAPIKey))
	if apiKey == "" {
		return ICloudRemoteMailbox{}, session, errCode("apple_account_api_key_missing", "Apple Account 管理态缺少 api_key，请重新完成 Apple Account 登录流程", true)
	}
	loginState.APIKey = apiKey
	session = withAppleAccountLoginState(session, loginState)
	label = strings.TrimSpace(label)
	if label == "" {
		label = "UPI-" + time.Now().Format("0102-150405")
	}
	note = strings.TrimSpace(note)

	var generated struct {
		EmailAddress string `json:"emailAddress"`
	}
	generatedRaw, err := c.callAppleAccountRaw(ctx, &loginState, apiKey, http.MethodPost, "/account/manage/email/private/add", map[string]any{}, &generated)
	if err != nil {
		return ICloudRemoteMailbox{}, withAppleAccountLoginState(session, loginState), err
	}
	if strings.TrimSpace(generated.EmailAddress) == "" {
		return ICloudRemoteMailbox{}, withAppleAccountLoginState(session, loginState), errCode("apple_account_generate_empty", "Apple Account 未返回候选隐私邮箱；"+appleAccountRawResponseDetail("生成候选隐私邮箱", generatedRaw), true)
	}

	var completed struct {
		EmailAddress string `json:"emailAddress"`
		Label        string `json:"label"`
		Note         string `json:"note"`
		ID           string `json:"id"`
		Active       bool   `json:"active"`
	}
	completedRaw, err := c.callAppleAccountRaw(ctx, &loginState, apiKey, http.MethodPut, "/account/manage/email/private/add/complete", map[string]string{
		"emailAddress": generated.EmailAddress,
		"label":        label,
		"note":         note,
	}, &completed)
	if err != nil {
		return ICloudRemoteMailbox{}, withAppleAccountLoginState(session, loginState), err
	}

	remote := ICloudRemoteMailbox{
		AnonymousID: strings.TrimSpace(completed.ID),
		Email:       strings.ToLower(strings.TrimSpace(firstNonEmpty(completed.EmailAddress, generated.EmailAddress))),
		Label:       strings.TrimSpace(firstNonEmpty(completed.Label, label)),
		Note:        strings.TrimSpace(firstNonEmpty(completed.Note, note, "created by Apple Account private email API")),
		IsActive:    completed.Active,
		Origin:      "APPLE_ACCOUNT",
	}
	if remote.AnonymousID != "" {
		var confirmed struct {
			EmailAddress   string `json:"emailAddress"`
			Label          string `json:"label"`
			Note           string `json:"note"`
			ID             string `json:"id"`
			ForwardToEmail string `json:"forwardToEmail"`
			Active         bool   `json:"active"`
		}
		path := "/account/manage/email/private/" + url.PathEscape(remote.AnonymousID) + ".em"
		if err := c.callAppleAccount(ctx, &loginState, apiKey, http.MethodGet, path, nil, &confirmed); err == nil {
			remote.Email = strings.ToLower(strings.TrimSpace(firstNonEmpty(confirmed.EmailAddress, remote.Email)))
			remote.Label = strings.TrimSpace(firstNonEmpty(confirmed.Label, remote.Label))
			remote.Note = strings.TrimSpace(firstNonEmpty(confirmed.Note, remote.Note))
			remote.ForwardToEmail = strings.TrimSpace(confirmed.ForwardToEmail)
			remote.IsActive = confirmed.Active
		}
	}
	if remote.Email == "" {
		return ICloudRemoteMailbox{}, session, errCode("apple_account_create_empty", "Apple Account 创建后未返回隐私邮箱；"+appleAccountRawResponseDetail("确认创建隐私邮箱", completedRaw), true)
	}
	markAppleAccountManageOK(&loginState)
	session = withAppleAccountLoginState(session, loginState)
	return remote, session, nil
}

func (c *ICloudClient) RefreshAppleAccountManageState(ctx context.Context, loginState LoginState) (LoginState, error) {
	release, err := acquireAppleAccountOperationGate(ctx, appleAccountOperationKey(ICloudSession{}, loginState))
	if err != nil {
		return loginState, err
	}
	defer release()
	return c.refreshAppleAccountManageStateUnlocked(ctx, loginState)
}

func (c *ICloudClient) KeepAliveAppleAccountManageState(ctx context.Context, loginState LoginState) (LoginState, error) {
	release, err := acquireAppleAccountOperationGate(ctx, appleAccountOperationKey(ICloudSession{}, loginState))
	if err != nil {
		return loginState, err
	}
	defer release()
	return c.keepAliveAppleAccountManageStateUnlocked(ctx, loginState)
}

func (c *ICloudClient) keepAliveAppleAccountManageStateUnlocked(ctx context.Context, loginState LoginState) (LoginState, error) {
	if strings.TrimSpace(loginState.Scnt) == "" {
		return loginState, errCode("apple_account_session_missing", "当前登录态缺少 Apple Account 管理态，请重新协议登录", true)
	}
	loginState, err := c.refreshAppleAccountManageStateUnlocked(ctx, loginState)
	if err != nil {
		return loginState, err
	}
	if _, err := c.callAppleAccountRaw(ctx, &loginState, loginState.APIKey, http.MethodGet, "/account/manage/forwardemail", nil, nil); err != nil {
		return loginState, err
	}
	if _, err := c.callAppleAccountRaw(ctx, &loginState, loginState.APIKey, http.MethodPost, "/v2/jslogs", appleAccountJSLogBody(), nil); err != nil {
		if stopSessionRecovery(err) {
			return loginState, err
		}
		if os.Getenv("SAM_DEBUG_APPLE_ACCOUNT") == "1" {
			fmt.Fprintf(os.Stderr, "APPLE_ACCOUNT_DEBUG keepalive jslogs ignored err=%v\n", err)
		}
	}
	markAppleAccountManageOK(&loginState)
	return loginState, nil
}

func appleAccountJSLogBody() []map[string]any {
	return []map[string]any{{
		"eventId":       appleAccountJSLogEventID(),
		"timestamp":     time.Now().UnixMilli(),
		"eventType":     "custom",
		"componentName": "performance",
		"action":        "memory",
		"metadata": map[string]any{
			"domNodeCount":    0,
			"usedJSHeapSize":  0,
			"totalJSHeapSize": 0,
			"heapUtilization": 0,
			"createdAt":       0,
			"elapsedTime":     0,
			"marks": map[string]any{
				"startTime": 0,
			},
			"eventVersion": 1,
		},
	}}
}

func appleAccountKeepAliveDue(loginState LoginState, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		interval = appleAccountKeepAliveDefaultInterval
	}
	if loginState.LastCheckedAt.IsZero() {
		return true
	}
	return !now.Before(loginState.LastCheckedAt.Add(interval))
}

func (c *ICloudClient) refreshAppleAccountManageStateUnlocked(ctx context.Context, loginState LoginState) (LoginState, error) {
	if strings.TrimSpace(loginState.Scnt) == "" {
		return loginState, errCode("apple_account_session_missing", "当前登录态缺少 Apple Account 管理态，请重新协议登录", true)
	}
	var token struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if err := c.callAppleAccount(ctx, &loginState, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); err != nil {
		return loginState, err
	}
	markAppleAccountManageTokenTTL(&loginState, token.TimeOutInterval, time.Now())
	if token.TimeOutInterval <= 0 {
		if err := c.warmAppleAccountPortal(ctx, &loginState); err == nil {
			token = struct {
				TimeOutInterval int `json:"timeOutInterval"`
			}{}
			withoutScnt := loginState
			withoutScnt.Scnt = ""
			if err := c.callAppleAccount(ctx, &withoutScnt, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); err == nil {
				loginState = withoutScnt
				markAppleAccountManageTokenTTL(&loginState, token.TimeOutInterval, time.Now())
			} else if stopSessionRecovery(err) {
				withoutScnt.Scnt = firstNonEmpty(withoutScnt.Scnt, loginState.Scnt)
				return withoutScnt, err
			} else if err := c.callAppleAccount(ctx, &loginState, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); err == nil {
				markAppleAccountManageTokenTTL(&loginState, token.TimeOutInterval, time.Now())
			} else if stopSessionRecovery(err) {
				return loginState, err
			}
		} else if stopSessionRecovery(err) {
			return loginState, err
		}
	}
	tokenScnt := strings.TrimSpace(loginState.Scnt)
	if err := c.loadAppleAccountManageAPIKey(ctx, &loginState); err == nil {
		markAppleAccountManageOK(&loginState)
		return loginState, nil
	} else if stopSessionRecovery(err) {
		return loginState, err
	}
	if err := c.warmAppleAccountPortal(ctx, &loginState); err != nil {
		return loginState, err
	}
	withoutScnt := loginState
	withoutScnt.Scnt = ""
	if err := c.callAppleAccount(ctx, &withoutScnt, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); err == nil {
		markAppleAccountManageTokenTTL(&withoutScnt, token.TimeOutInterval, time.Now())
		loginState = withoutScnt
	} else if stopSessionRecovery(err) {
		withoutScnt.Scnt = firstNonEmpty(withoutScnt.Scnt, loginState.Scnt)
		return withoutScnt, err
	} else if err := c.callAppleAccount(ctx, &loginState, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); err != nil {
		if tokenScnt == "" || stopSessionRecovery(err) {
			return loginState, err
		}
		loginState.Scnt = tokenScnt
		if retryErr := c.callAppleAccount(ctx, &loginState, "", http.MethodGet, "/account/manage/gs/ws/token", nil, &token); retryErr != nil {
			return loginState, retryErr
		}
		markAppleAccountManageTokenTTL(&loginState, token.TimeOutInterval, time.Now())
	} else {
		markAppleAccountManageTokenTTL(&loginState, token.TimeOutInterval, time.Now())
	}
	if err := c.loadAppleAccountManageAPIKey(ctx, &loginState); err != nil {
		return loginState, err
	}
	markAppleAccountManageOK(&loginState)
	return loginState, nil
}

func appleAccountOperationKey(session ICloudSession, loginState LoginState) string {
	identity := firstNonEmpty(session.AccountID, session.DSID, session.AppleID, loginState.SessionID, loginState.Scnt, loginState.APIKey, "default")
	return "account:" + identity
}

func acquireAppleAccountOperationGate(ctx context.Context, key string) (func(), error) {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "global:default"
	}
	appleAccountOperationMu.Lock()
	gate := appleAccountOperationGates[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		appleAccountOperationGates[key] = gate
	}
	appleAccountOperationMu.Unlock()

	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *ICloudClient) loadAppleAccountManageAPIKey(ctx context.Context, loginState *LoginState) error {
	var manage struct {
		APIKey string `json:"apiKey"`
	}
	if err := c.callAppleAccount(ctx, loginState, "", http.MethodGet, "/account/manage", nil, &manage); err != nil {
		return err
	}
	if apiKey := strings.TrimSpace(manage.APIKey); apiKey != "" {
		loginState.APIKey = apiKey
	}
	if strings.TrimSpace(loginState.APIKey) == "" {
		return errCode("apple_account_api_key_missing", "Apple Account 管理接口未返回 api_key，请重新完成 Apple Account 登录流程", true)
	}
	return nil
}

func (c *ICloudClient) warmAppleAccountPortal(ctx context.Context, loginState *LoginState) error {
	if _, err := c.callAppleAccountPortal(ctx, loginState, "/account/manage/section/privacy", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7", false, "document", "navigate"); err != nil {
		return err
	}
	data, err := c.callAppleAccountPortal(ctx, loginState, "/bootstrap/portal", "application/json, text/plain, */*", true, "empty", "cors")
	if err != nil {
		return err
	}
	var portal struct {
		TimeOutInterval int `json:"timeOutInterval"`
	}
	if json.Unmarshal(data, &portal) == nil {
		markAppleAccountManageTokenTTL(loginState, portal.TimeOutInterval, time.Now())
	}
	return nil
}

func (c *ICloudClient) callAppleAccountPortal(ctx context.Context, loginState *LoginState, path, accept string, jsonContent bool, secFetchDest, secFetchMode string) ([]byte, error) {
	var data []byte
	err := retryAppleTransient(ctx, func() error {
		next, err := c.callAppleAccountPortalOnce(ctx, loginState, path, accept, jsonContent, secFetchDest, secFetchMode)
		if err != nil {
			return err
		}
		data = next
		return nil
	})
	return data, err
}

func (c *ICloudClient) callAppleAccountPortalOnce(ctx context.Context, loginState *LoginState, path, accept string, jsonContent bool, secFetchDest, secFetchMode string) ([]byte, error) {
	if loginState == nil {
		return nil, errCode("apple_account_session_missing", "当前登录态缺少 Apple Account 管理态，请重新协议登录", true)
	}
	base, err := url.Parse(strings.TrimRight(appleAccountPortalBaseForState(*loginState), "/") + "/")
	if err != nil {
		return nil, err
	}
	rel := &url.URL{Path: strings.TrimLeft(path, "/")}
	rawURL := base.ResolveReference(rel).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	userAgent := firstNonEmpty(loginState.UserAgent, appleAccountManageUserAgent)
	req.Header.Set("Accept", accept)
	if jsonContent {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Referer", strings.TrimRight(firstNonEmpty(loginState.Origin, appleAccountManageOrigin), "/")+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", secFetchMode)
	req.Header.Set("Sec-Fetch-Dest", secFetchDest)
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	if cookie := cookieHeader(loginState.Cookies, rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if jsonContent {
		req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
		req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
		req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(userAgent))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if os.Getenv("SAM_DEBUG_APPLE_ACCOUNT") == "1" {
		fmt.Fprintf(os.Stderr, "APPLE_ACCOUNT_PORTAL_DEBUG method=GET path=%s status=%d req_cookie_len=%d req_scnt=%s res_scnt=%s res_session=%s set_cookie=%d body=%q\n",
			path,
			resp.StatusCode,
			len(req.Header.Get("Cookie")),
			appleDebugFingerprint(req.Header.Get("scnt")),
			appleDebugFingerprint(resp.Header.Get("scnt")),
			appleDebugFingerprint(resp.Header.Get("X-Apple-ID-Session-Id")),
			len(resp.Cookies()),
			appleDebugBody(data),
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, withHTTPDetails(appleAccountAPIError(resp.StatusCode, data, appleAccountRequestStage(http.MethodGet, path)), resp)
	}
	mergeSessionCookies(&loginState.Cookies, resp.Request.URL, resp.Cookies())
	updateAppleAccountLoginStateFromHeaders(loginState, resp.Header)
	return data, nil
}
