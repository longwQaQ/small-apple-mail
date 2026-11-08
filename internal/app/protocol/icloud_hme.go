package protocol

import (
	"bytes"
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

func (c *ICloudClient) ListPrivacyMailboxes(ctx context.Context, session ICloudSession) ([]ICloudRemoteMailbox, error) {
	items, _, err := c.ListPrivacyMailboxesWithSession(ctx, session)
	return items, err
}

func (c *ICloudClient) ListPrivacyMailboxesWithSession(ctx context.Context, session ICloudSession) ([]ICloudRemoteMailbox, ICloudSession, error) {
	items, err := c.listPrivacyMailboxes(ctx, &session)
	return items, session, err
}

func (c *ICloudClient) listPrivacyMailboxes(ctx context.Context, session *ICloudSession) ([]ICloudRemoteMailbox, error) {
	if strings.TrimSpace(session.PremiumMailBaseURL) == "" || strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return nil, errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	var out struct {
		HMEEmails json.RawMessage `json:"hmeEmails"`
	}
	if err := retryAppleTransient(ctx, func() error {
		return c.callSession(ctx, session, http.MethodGet, "/v2/hme/list", nil, &out)
	}); err != nil {
		return nil, err
	}
	return parseICloudRemoteMailboxSnapshot(out.HMEEmails)
}

type iCloudRemoteMailboxItem struct {
	AnonymousID     string          `json:"anonymousId"`
	HME             string          `json:"hme"`
	Label           string          `json:"label"`
	ForwardToEmail  string          `json:"forwardToEmail"`
	IsActive        bool            `json:"isActive"`
	Origin          string          `json:"origin"`
	Note            string          `json:"note"`
	CreateTimestamp json.RawMessage `json:"createTimestamp"`
	CreatedAt       json.RawMessage `json:"createdAt"`
}

func parseICloudRemoteMailboxSnapshot(raw json.RawMessage) ([]ICloudRemoteMailbox, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errCode("icloud_hme_snapshot_invalid", "iCloud 隐私邮箱快照缺少 hmeEmails 数组", true)
	}
	var items []iCloudRemoteMailboxItem
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errCode("icloud_hme_snapshot_invalid", "iCloud 隐私邮箱快照中的 hmeEmails 不是有效数组", true)
	}
	remotes := make([]ICloudRemoteMailbox, 0, len(items))
	emails := make(map[string]struct{}, len(items))
	anonymousIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		email := strings.ToLower(strings.TrimSpace(item.HME))
		if email == "" {
			return nil, errCode("icloud_hme_snapshot_invalid", "iCloud 隐私邮箱快照包含空邮箱地址", true)
		}
		if _, exists := emails[email]; exists {
			return nil, errCode("icloud_hme_snapshot_invalid", "iCloud 隐私邮箱快照包含重复邮箱地址", true)
		}
		emails[email] = struct{}{}
		anonymousID := strings.TrimSpace(item.AnonymousID)
		if anonymousID != "" {
			if _, exists := anonymousIDs[anonymousID]; exists {
				return nil, errCode("icloud_hme_snapshot_invalid", "iCloud 隐私邮箱快照包含重复远端标识", true)
			}
			anonymousIDs[anonymousID] = struct{}{}
		}
		remotes = append(remotes, ICloudRemoteMailbox{
			AnonymousID:    anonymousID,
			Email:          email,
			Label:          strings.TrimSpace(item.Label),
			ForwardToEmail: strings.TrimSpace(item.ForwardToEmail),
			IsActive:       item.IsActive,
			Origin:         strings.TrimSpace(item.Origin),
			Note:           strings.TrimSpace(item.Note),
			CreatedAt:      parseICloudRemoteTime(firstNonEmptyRaw(item.CreateTimestamp, item.CreatedAt)),
		})
	}
	return remotes, nil
}

func firstNonEmptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(bytes.TrimSpace(value)) > 0 && string(bytes.TrimSpace(value)) != "null" {
			return value
		}
	}
	return nil
}

func parseICloudRemoteTime(raw json.RawMessage) time.Time {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var number json.Number
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return time.Time{}
		}
		text = strings.TrimSpace(text)
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return parsed.UTC()
		}
		number = json.Number(text)
	} else {
		number = json.Number(string(raw))
	}
	value, err := number.Int64()
	if err != nil || value <= 0 {
		return time.Time{}
	}
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func (c *ICloudClient) generate(ctx context.Context, session *ICloudSession) (string, error) {
	var out struct {
		HME string `json:"hme"`
	}
	if err := c.callSession(ctx, session, http.MethodPost, "/v1/hme/generate", map[string]string{
		"langCode": "zh-cn",
	}, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.HME) == "" {
		return "", errCode("icloud_generate_empty", "iCloud 未返回候选隐私邮箱", true)
	}
	return strings.TrimSpace(out.HME), nil
}

func (c *ICloudClient) reserve(ctx context.Context, session *ICloudSession, hme, label, note string) (ICloudRemoteMailbox, error) {
	var out struct {
		HME struct {
			AnonymousID    string `json:"anonymousId"`
			HME            string `json:"hme"`
			Label          string `json:"label"`
			Note           string `json:"note"`
			ForwardToEmail string `json:"forwardToEmail"`
			IsActive       bool   `json:"isActive"`
		} `json:"hme"`
	}
	if err := c.callSession(ctx, session, http.MethodPost, "/v1/hme/reserve", map[string]string{
		"hme":   hme,
		"label": label,
		"note":  note,
	}, &out); err != nil {
		return ICloudRemoteMailbox{}, err
	}
	return ICloudRemoteMailbox{
		AnonymousID:    out.HME.AnonymousID,
		Email:          out.HME.HME,
		Label:          out.HME.Label,
		Note:           out.HME.Note,
		ForwardToEmail: out.HME.ForwardToEmail,
		IsActive:       out.HME.IsActive,
		Origin:         "ICLOUD_WEB",
	}, nil
}

func (c *ICloudClient) callAppleAccount(ctx context.Context, loginState *LoginState, apiKey, method, path string, body any, result any) error {
	_, err := c.callAppleAccountRaw(ctx, loginState, apiKey, method, path, body, result)
	return err
}

func (c *ICloudClient) fetchAppleAccountManageTokenScnt(ctx context.Context, loginState LoginState, result any) (string, error) {
	var scnt string
	err := retryAppleTransient(ctx, func() error {
		next, err := c.fetchAppleAccountManageTokenScntOnce(ctx, loginState, result)
		if strings.TrimSpace(next) != "" {
			scnt = next
		}
		if err != nil {
			return err
		}
		return nil
	})
	return scnt, err
}

func (c *ICloudClient) fetchAppleAccountManageTokenScntOnce(ctx context.Context, loginState LoginState, result any) (string, error) {
	base, err := url.Parse(strings.TrimRight(appleAccountManageBaseForState(loginState), "/") + "/")
	if err != nil {
		return "", err
	}
	rawURL := base.ResolveReference(&url.URL{Path: "account/manage/gs/ws/token"}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	origin := strings.TrimRight(firstNonEmpty(loginState.Origin, appleAccountManageOriginForHost(loginState.Host), appleAccountManageOrigin), "/")
	userAgent := firstNonEmpty(loginState.UserAgent, appleAccountManageUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(userAgent))
	req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
	req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
	if cookie := cookieHeader(loginState.Cookies, rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	scnt := strings.TrimSpace(resp.Header.Get("scnt"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return scnt, withHTTPDetails(appleAccountAPIError(resp.StatusCode, data, appleAccountRequestStage(http.MethodGet, "/account/manage/gs/ws/token")), resp)
	}
	if result != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, result); err != nil {
			return scnt, errCode("apple_account_bad_response", "Apple Account 返回无法解析；"+appleAccountResponseDetail(appleAccountRequestStage(http.MethodGet, "/account/manage/gs/ws/token"), data), true)
		}
	}
	return scnt, nil
}

func (c *ICloudClient) callAppleAccountRaw(ctx context.Context, loginState *LoginState, apiKey, method, path string, body any, result any) (appleAccountRawResponse, error) {
	// 写请求断线时结果可能已生效，不能按网络错误自动重放。
	if method != http.MethodGet {
		return c.callAppleAccountRawOnce(ctx, loginState, apiKey, method, path, body, result)
	}
	var raw appleAccountRawResponse
	err := retryAppleTransient(ctx, func() error {
		next, err := c.callAppleAccountRawOnce(ctx, loginState, apiKey, method, path, body, result)
		if err != nil {
			return err
		}
		raw = next
		return nil
	})
	return raw, err
}

func (c *ICloudClient) callAppleAccountRawOnce(ctx context.Context, loginState *LoginState, apiKey, method, path string, body any, result any) (appleAccountRawResponse, error) {
	if loginState == nil {
		return appleAccountRawResponse{}, errCode("apple_account_session_missing", "当前登录态缺少 Apple Account 管理态，请重新协议登录", true)
	}
	base, err := url.Parse(strings.TrimRight(appleAccountManageBaseForState(*loginState), "/") + "/")
	if err != nil {
		return appleAccountRawResponse{}, err
	}
	rel := &url.URL{Path: strings.TrimLeft(path, "/")}
	rawURL := base.ResolveReference(rel).String()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return appleAccountRawResponse{}, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return appleAccountRawResponse{}, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	origin := strings.TrimRight(firstNonEmpty(loginState.Origin, appleAccountManageOriginForHost(loginState.Host), appleAccountManageOrigin), "/")
	userAgent := firstNonEmpty(loginState.UserAgent, appleAccountManageUserAgent)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", appleAccountManageLanguage+",en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", appleAccountManagePlatform)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	if scnt := strings.TrimSpace(loginState.Scnt); scnt != "" {
		req.Header.Set("scnt", scnt)
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("X-Apple-Api-Key", apiKey)
	}
	req.Header.Set("X-Apple-I-FD-Client-Info", appleAccountFDClientInfo(userAgent))
	req.Header.Set("X-Apple-I-Request-Context", appleAccountManageRequestCtx)
	req.Header.Set("X-Apple-I-TimeZone", appleAccountManageTimeZone)
	if cookie := cookieHeader(loginState.Cookies, rawURL); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return appleAccountRawResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	raw := appleAccountRawResponse{StatusCode: resp.StatusCode, Body: data}
	if err != nil {
		return raw, err
	}
	if os.Getenv("SAM_DEBUG_APPLE_ACCOUNT") == "1" {
		fmt.Fprintf(os.Stderr, "APPLE_ACCOUNT_DEBUG method=%s path=%s status=%d req_cookie_len=%d req_scnt=%s res_scnt=%s res_session=%s set_cookie=%d body=%q\n",
			method,
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
		if os.Getenv("SAM_DEBUG_APPLE_ACCOUNT") == "1" {
			fmt.Fprintf(os.Stderr, "APPLE_ACCOUNT_DEBUG method=%s path=%s status=%d body=%q\n", method, path, resp.StatusCode, appleDebugBody(data))
		}
		return raw, withHTTPDetails(appleAccountAPIError(resp.StatusCode, data, appleAccountRequestStage(method, path)), resp)
	}
	mergeSessionCookies(&loginState.Cookies, resp.Request.URL, resp.Cookies())
	updateAppleAccountLoginStateFromHeaders(loginState, resp.Header)
	if result != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, result); err != nil {
			return raw, errCode("apple_account_bad_response", "Apple Account 返回无法解析；"+appleAccountRawResponseDetail(appleAccountRequestStage(method, path), raw), true)
		}
	}
	return raw, nil
}
