package protocol

import "time"

const (
	LoginStateICloudWeb    = "icloud_web"
	LoginStateAppleAccount = "apple_account"
	LoginStateICloudIMAP   = "imap"
)

// Mailbox 是协议层执行远端邮件同步所需的最小邮箱投影。
type Mailbox struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	LastSyncAt  time.Time `json:"last_sync_at,omitempty"`
	LastSyncUID string    `json:"last_sync_uid,omitempty"`
}

// ICloudSession 保存与 Apple 和 iCloud 服务交互所需的当前登录态。
// 连接凭据按产品约定以明文 JSON 写入 SQLite；调用方不得将其写入日志、活动或 API 响应。
type ICloudSession struct {
	AccountID          string          `json:"account_id,omitempty"`
	SavedAt            time.Time       `json:"saved_at"`
	AppleID            string          `json:"apple_id,omitempty"`
	DSID               string          `json:"dsid"`
	ClientID           string          `json:"client_id"`
	ClientBuildNumber  string          `json:"client_build_number"`
	MasteringNumber    string          `json:"client_mastering_number"`
	PremiumMailBaseURL string          `json:"premium_mail_base_url"`
	MailGatewayBaseURL string          `json:"mail_gateway_base_url,omitempty"`
	MailBaseURL        string          `json:"mail_base_url,omitempty"`
	Host               string          `json:"host"`
	IsICloudPlus       bool            `json:"is_icloud_plus"`
	CanCreateHME       bool            `json:"can_create_hme"`
	Cookies            []SessionCookie `json:"cookies"`
	WebAuth            *ICloudWebAuth  `json:"web_auth,omitempty"`
	LoginStates        []LoginState    `json:"login_states,omitempty"`
	Note               string          `json:"note,omitempty"`
	LastCheckedAt      time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK        bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage  string          `json:"last_status_message,omitempty"`
}

// ICloudWebAuth 仅保存 Apple 已签发的会话凭据，不保存账号密码。
type ICloudWebAuth struct {
	SessionToken   string `json:"session_token"`
	TrustToken     string `json:"trust_token,omitempty"`
	AccountCountry string `json:"account_country,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Scnt           string `json:"scnt,omitempty"`
}

type LoginState struct {
	Kind              string          `json:"kind"`
	Host              string          `json:"host,omitempty"`
	Origin            string          `json:"origin,omitempty"`
	SavedAt           time.Time       `json:"saved_at,omitempty"`
	Cookies           []SessionCookie `json:"cookies,omitempty"`
	Scnt              string          `json:"scnt,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	APIKey            string          `json:"api_key,omitempty"`
	DataAccessToken   string          `json:"data_access_token,omitempty"`
	UserAgent         string          `json:"user_agent,omitempty"`
	Note              string          `json:"note,omitempty"`
	IMAPEmail         string          `json:"imap_email,omitempty"`
	IMAPUsername      string          `json:"imap_username,omitempty"`
	IMAPHost          string          `json:"imap_host,omitempty"`
	IMAPPort          int             `json:"imap_port,omitempty"`
	IMAPAppPassword   string          `json:"imap_app_password,omitempty"`
	IMAPLastSyncAt    time.Time       `json:"imap_last_sync_at,omitempty"`
	IMAPLastSyncUID   string          `json:"imap_last_sync_uid,omitempty"`
	IMAPUIDValidity   string          `json:"imap_uid_validity,omitempty"`
	ManageExpiresAt   time.Time       `json:"manage_expires_at,omitempty"`
	LastCheckedAt     time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK       bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage string          `json:"last_status_message,omitempty"`
}

type SessionCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
}
