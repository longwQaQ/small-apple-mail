package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RefreshConnection 返回需要一起保存的会话与有效期；IMAP 不产生新的凭据。
func RefreshConnection(ctx context.Context, kind, secretJSON string) (string, time.Time, error) {
	if kind == LoginStateICloudIMAP {
		return secretJSON, time.Time{}, CheckConnection(ctx, kind, secretJSON)
	}
	var session ICloudSession
	if err := json.Unmarshal([]byte(secretJSON), &session); err != nil {
		return secretJSON, time.Time{}, errCode("session_login_required", "连接凭据格式无效，请更新连接", false)
	}
	var updated ICloudSession
	var err error
	switch kind {
	case LoginStateAppleAccount:
		state, ok := appleAccountLoginState(session)
		if !ok {
			return secretJSON, time.Time{}, errCode("apple_account_session_missing", "未保存备用创建通道登录态，请更新连接", false)
		}
		client := NewICloudClient()
		client.client.CheckRedirect = refuseSessionRedirect
		state, err = client.KeepAliveAppleAccountManageState(ctx, state)
		updated = withAppleAccountLoginState(session, state)
	case LoginStateICloudWeb:
		updated, err = NewAppleAuthClient().RefreshICloudSession(ctx, session)
	default:
		return secretJSON, time.Time{}, fmt.Errorf("不支持的连接类型")
	}
	encoded, encodeErr := json.Marshal(updated)
	if encodeErr != nil {
		return secretJSON, time.Time{}, encodeErr
	}
	return string(encoded), SessionExpiresAt(kind, updated), err
}

func SessionExpiresAt(kind string, session ICloudSession) time.Time {
	if kind == LoginStateAppleAccount {
		state, _ := appleAccountLoginState(session)
		return state.ManageExpiresAt
	}
	var expires time.Time
	for _, cookie := range session.Cookies {
		if cookie.Name != "X-APPLE-WEBAUTH-USER" || cookie.Expires <= 0 || cookie.Value == "" {
			continue
		}
		at := time.Unix(int64(cookie.Expires), 0)
		if expires.IsZero() || at.Before(expires) {
			expires = at
		}
	}
	return expires
}

// RefreshICloudSession 优先验证现有 Cookie，必要时仅尝试一次已签发 token 换会话。
// 不发起密码登录或验证码挑战；旧数据没有 token 时由用户重新登录。
func (c *AppleAuthClient) RefreshICloudSession(ctx context.Context, session ICloudSession) (ICloudSession, error) {
	// 后台维护只接受下面显式处理的地区切换，不自动跟随任意 Location。
	client := *c.httpClient
	client.CheckRedirect = refuseSessionRedirect
	c = &AppleAuthClient{httpClient: &client}
	updated, err := c.refreshICloudSession(ctx, session)
	var redirect appleDomainRedirectError
	if !errors.As(err, &redirect) {
		return updated, err
	}
	if redirect.Host == session.Host {
		return session, errCode("session_login_required", "Apple 会话验证失败，请更新连接重新登录", false)
	}
	session = updated
	session.Host = redirect.Host
	updated, err = c.refreshICloudSession(ctx, session)
	if errors.As(err, &redirect) {
		return session, errCode("session_login_required", "Apple 会话地区验证失败，请更新连接重新登录", false)
	}
	return updated, err
}

func refuseSessionRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (c *AppleAuthClient) refreshICloudSession(ctx context.Context, session ICloudSession) (ICloudSession, error) {
	validation, err := (&ICloudSessionValidator{httpClient: c.httpClient}).Validate(ctx, session.Cookies, session.Host)
	if err != nil && !ClassifySessionError(err).RequiresLogin {
		return session, err
	}
	if err == nil {
		if err := checkSessionIdentity(session, validation.DSID); err != nil {
			return session, err
		}
		session = applyICloudValidation(session, validation)
		expires := SessionExpiresAt(LoginStateICloudWeb, session)
		if expires.IsZero() || expires.After(time.Now().Add(10*time.Minute)) || session.WebAuth == nil || session.WebAuth.SessionToken == "" {
			return session, nil
		}
	}
	if session.WebAuth == nil || strings.TrimSpace(session.WebAuth.SessionToken) == "" {
		return session, err
	}
	auth := session.WebAuth
	state := &appleAuthSession{Endpoints: appleAuthEndpointsForHost(session.Host), AppleID: session.AppleID, ClientID: session.ClientID, Cookies: append([]SessionCookie(nil), session.Cookies...), SessionToken: auth.SessionToken, TrustToken: auth.TrustToken, AccountCountry: auth.AccountCountry, SessionID: auth.SessionID, Scnt: auth.Scnt}
	updated, err := c.authWithTokenAndValidate(ctx, state)
	if err != nil {
		// token 交换已成功但最终验证暂时失败时，也要保留服务器轮换的凭据。
		session.Cookies = state.cloneCookies()
		session.WebAuth = &ICloudWebAuth{SessionToken: state.SessionToken, TrustToken: state.TrustToken, AccountCountry: state.AccountCountry, SessionID: state.SessionID, Scnt: state.Scnt}
		return session, err
	}
	if err := checkSessionIdentity(session, updated.DSID); err != nil {
		return session, err
	}
	updated.AccountID = session.AccountID
	return updated, nil
}

func checkSessionIdentity(session ICloudSession, dsid string) error {
	if session.DSID != "" && session.DSID != dsid {
		return errCode("session_login_required", "Apple 返回的账号身份与原连接不一致，请重新登录", false)
	}
	return nil
}

func applyICloudValidation(session ICloudSession, v validateResult) ICloudSession {
	session.DSID, session.ClientID = v.DSID, v.ClientID
	session.ClientBuildNumber, session.MasteringNumber = v.ClientBuildNumber, v.MasteringNumber
	session.PremiumMailBaseURL, session.MailGatewayBaseURL, session.MailBaseURL = strings.TrimRight(v.PremiumMailBaseURL, "/"), strings.TrimRight(v.MailGatewayBaseURL, "/"), strings.TrimRight(v.MailBaseURL, "/")
	session.IsICloudPlus, session.CanCreateHME = v.IsICloudPlus, v.CanCreateHME
	session.Cookies = v.Cookies
	session.WebAuth = mergeWebAuth(session.WebAuth, v.WebAuth)
	session.LastCheckedAt, session.LastCheckOK = time.Now().UTC(), true
	for i := range session.LoginStates {
		if session.LoginStates[i].Kind == LoginStateICloudWeb {
			session.LoginStates[i].Cookies = append([]SessionCookie(nil), v.Cookies...)
		}
	}
	return session
}

func mergeWebAuth(previous, next *ICloudWebAuth) *ICloudWebAuth {
	if next == nil || *next == (ICloudWebAuth{}) {
		return previous
	}
	if previous == nil {
		copy := *next
		return &copy
	}
	copy := *previous
	copy.SessionToken = firstNonEmpty(next.SessionToken, copy.SessionToken)
	copy.TrustToken = firstNonEmpty(next.TrustToken, copy.TrustToken)
	copy.AccountCountry = firstNonEmpty(next.AccountCountry, copy.AccountCountry)
	copy.SessionID = firstNonEmpty(next.SessionID, copy.SessionID)
	copy.Scnt = firstNonEmpty(next.Scnt, copy.Scnt)
	return &copy
}
