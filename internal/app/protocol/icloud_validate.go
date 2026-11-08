package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ICloudSessionValidator struct {
	httpClient *http.Client
}

type validateResult struct {
	Cookies            []SessionCookie
	WebAuth            *ICloudWebAuth
	AppleID            string
	DSID               string
	ClientID           string
	ClientBuildNumber  string
	MasteringNumber    string
	PremiumMailBaseURL string
	MailGatewayBaseURL string
	MailBaseURL        string
	IsICloudPlus       bool
	CanCreateHME       bool
}

func NewICloudSessionValidator() *ICloudSessionValidator {
	return &ICloudSessionValidator{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

func (c *ICloudSessionValidator) Validate(ctx context.Context, cookies []SessionCookie, defaultHost string) (validateResult, error) {
	host := strings.TrimSpace(defaultHost)
	if host == "" {
		host = "www.icloud.com.cn"
	}
	setupHost := "setup.icloud.com.cn"
	if strings.HasSuffix(host, "icloud.com") && !strings.HasSuffix(host, "icloud.com.cn") {
		setupHost = "setup.icloud.com"
	}

	clientID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return validateResult{}, err
	}
	buildNumber := "2622Build20"
	masteringNumber := buildNumber

	u := url.URL{
		Scheme: "https",
		Host:   setupHost,
		Path:   "/setup/ws/1/validate",
	}
	q := u.Query()
	q.Set("clientBuildNumber", buildNumber)
	q.Set("clientMasteringNumber", masteringNumber)
	q.Set("clientId", clientID)
	q.Set("requestId", requestID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader("null"))
	if err != nil {
		return validateResult{}, err
	}
	session := ICloudSession{Host: host, Cookies: cookies}
	setICloudFetchHeaders(req, session, "*/*", "text/plain;charset=UTF-8")
	if cookie := cookieHeader(cookies, u.String()); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return validateResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return validateResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if redirect, ok := parseAppleDomainRedirect(resp.StatusCode, data); ok {
			return validateResult{}, redirect
		}
		return validateResult{}, withHTTPDetails(errCode("icloud_validate_failed", fmt.Sprintf("iCloud 登录态校验失败，HTTP %d: %s", resp.StatusCode, appleDebugBody(data)), true), resp)
	}
	var account appleAccountInfo
	if err := json.Unmarshal(data, &account); err != nil {
		return validateResult{}, errCode("icloud_validate_bad_response", "iCloud 登录态校验返回无法解析", true)
	}
	if account.requiresTwoFactor() {
		return validateResult{}, errCode("session_login_required", "Apple 要求重新完成双重认证，请更新连接", false)
	}
	if account.DSInfo.DSID == "" || account.Webservices["premiummailsettings"].URL == "" {
		return validateResult{}, errCode("icloud_validate_incomplete", "iCloud 登录态未返回账号或隐私邮箱服务信息", true)
	}
	for _, name := range []string{"premiummailsettings", "mccgateway", "mail"} {
		service := account.Webservices[name]
		if service.URL != "" && !validICloudServiceURL(service.URL) {
			return validateResult{}, errCode("icloud_validate_bad_response", "iCloud 返回的服务地址无效", false)
		}
	}
	cookies = append([]SessionCookie(nil), cookies...)
	mergeSessionCookies(&cookies, resp.Request.URL, resp.Cookies())
	premium := account.Webservices["premiummailsettings"].URL
	mailGateway := account.Webservices["mccgateway"].URL
	mail := account.Webservices["mail"].URL
	appleID := account.DSInfo.AppleID
	if appleID == "" {
		appleID = account.DSInfo.PrimaryEmail
	}
	return validateResult{
		Cookies:            cookies,
		WebAuth:            &ICloudWebAuth{SessionToken: resp.Header.Get("X-Apple-Session-Token"), TrustToken: resp.Header.Get("X-Apple-TwoSV-Trust-Token"), AccountCountry: resp.Header.Get("X-Apple-ID-Account-Country"), SessionID: resp.Header.Get("X-Apple-ID-Session-Id"), Scnt: resp.Header.Get("scnt")},
		AppleID:            appleID,
		DSID:               account.DSInfo.DSID,
		ClientID:           clientID,
		ClientBuildNumber:  buildNumber,
		MasteringNumber:    masteringNumber,
		PremiumMailBaseURL: premium,
		MailGatewayBaseURL: mailGateway,
		MailBaseURL:        mail,
		IsICloudPlus:       account.DSInfo.IsHideMyEmailSubscriptionActive,
		CanCreateHME:       account.DSInfo.IsHideMyEmailFeatureAvailable,
	}, nil
}

func validICloudServiceURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "icloud.com" || host == "icloud.com.cn" || strings.HasSuffix(host, ".icloud.com") || strings.HasSuffix(host, ".icloud.com.cn")
}
