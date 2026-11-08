package domain

import "time"

const (
	ConnectionAppleAccount = "apple_account"
	ConnectionICloudWeb    = "icloud_web"
	ConnectionIMAP         = "imap"

	ConnectionNotConfigured = "not_configured"
	ConnectionPending2FA    = "pending_2fa"
	ConnectionChecking      = "checking"
	ConnectionHealthy       = "healthy"
	ConnectionExpiring      = "expiring"
	ConnectionExpired       = "expired"
	ConnectionRateLimited   = "rate_limited"
	ConnectionFailed        = "failed"
	ConnectionDisabled      = "disabled"

	MailboxAvailable = "available"
	MailboxLeased    = "leased"
	MailboxDisabled  = "disabled"
	MailboxMissing   = "remote_missing"

	LeaseActive   = "active"
	LeaseReleased = "released"
	LeaseExpired  = "expired"
	LeaseRevoked  = "revoked"

	JobQueued             = "queued"
	JobRunning            = "running"
	JobSucceeded          = "succeeded"
	JobPartiallySucceeded = "partially_succeeded"
	JobFailed             = "failed"
	JobCancelling         = "cancelling"
	JobCancelled          = "cancelled"
	JobInterrupted        = "interrupted"
)

var ValidConnectionKinds = map[string]bool{
	ConnectionAppleAccount: true,
	ConnectionICloudWeb:    true,
	ConnectionIMAP:         true,
}

var ValidScopes = map[string]bool{
	"leases:read":  true,
	"leases:write": true,
	"codes:read":   true,
}

type Admin struct {
	ID                string    `json:"id"`
	Username          string    `json:"username"`
	PasswordHash      string    `json:"-"`
	PasswordChangedAt time.Time `json:"password_changed_at"`
	LastLoginAt       time.Time `json:"last_login_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type Session struct {
	ID                string
	TokenHash         string
	CSRFHash          string
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type Account struct {
	ID          string       `json:"id"`
	AppleID     string       `json:"apple_id"`
	DisplayName string       `json:"display_name"`
	Note        string       `json:"note"`
	Status      string       `json:"status"`
	Connections []Connection `json:"connections,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

type Connection struct {
	ID                  string    `json:"id"`
	AccountID           string    `json:"account_id"`
	Kind                string    `json:"kind"`
	Status              string    `json:"status"`
	Host                string    `json:"host,omitempty"`
	SecretJSON          string    `json:"-"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	ExpiresAt           time.Time `json:"expires_at,omitempty"`
	NextCheckAt         time.Time `json:"next_check_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	StatusMessage       string    `json:"status_message,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type Mailbox struct {
	AllocationMode    string    `json:"allocation_mode"`
	Purpose           string    `json:"purpose"`
	ID                string    `json:"id"`
	AccountID         string    `json:"account_id"`
	Email             string    `json:"email"`
	RemoteAnonymousID string    `json:"remote_anonymous_id,omitempty"`
	Label             string    `json:"label"`
	Note              string    `json:"note"`
	Tags              []string  `json:"tags"`
	Status            string    `json:"status"`
	RemoteStatus      string    `json:"remote_status"`
	LocalEnabled      bool      `json:"local_enabled"`
	RemotePresent     bool      `json:"remote_present"`
	RemoteEnabled     bool      `json:"remote_enabled"`
	LastLeasedAt      time.Time `json:"last_leased_at,omitempty"`
	LastSyncedAt      time.Time `json:"last_synced_at,omitempty"`
	LastCode          string    `json:"last_code,omitempty"`
	LastCodeAt        time.Time `json:"last_code_at,omitempty"`
	LastLoginURL      string    `json:"last_login_url,omitempty"`
	LastLoginURLAt    time.Time `json:"last_login_url_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Message struct {
	ID              string    `json:"id"`
	MailboxID       string    `json:"mailbox_id"`
	RemoteID        string    `json:"remote_id"`
	Sender          string    `json:"sender"`
	Subject         string    `json:"subject"`
	Code            string    `json:"code"`
	LoginURL        string    `json:"login_url,omitempty"`
	MatchRule       string    `json:"match_rule,omitempty"`
	Confidence      float64   `json:"confidence,omitempty"`
	Body            string    `json:"body,omitempty"`
	ReceivedAt      time.Time `json:"received_at"`
	SyncedAt        time.Time `json:"synced_at"`
	ConsumedAt      time.Time `json:"consumed_at,omitempty"`
	ConsumedLeaseID string    `json:"consumed_lease_id,omitempty"`
}

type APIClient struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	KeyPrefix  string    `json:"key_prefix"`
	KeyHash    string    `json:"-"`
	Status     string    `json:"status"`
	Scopes     []string  `json:"scopes"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Lease struct {
	ID             string    `json:"id"`
	MailboxID      string    `json:"mailbox_id"`
	APIClientID    string    `json:"api_client_id"`
	Project        string    `json:"project"`
	Purpose        string    `json:"purpose"`
	TokenHash      string    `json:"-"`
	IdempotencyKey string    `json:"-"`
	RequestHash    string    `json:"-"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	ReleasedAt     time.Time `json:"released_at,omitempty"`
}

type Job struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	Priority   int       `json:"priority"`
	Payload    string    `json:"payload,omitempty"`
	Progress   int       `json:"progress"`
	Attempts   int       `json:"attempts"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type JobEvent struct {
	ID        int64     `json:"id"`
	JobID     string    `json:"job_id"`
	Stage     string    `json:"stage"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Schedule struct {
	ID                  string    `json:"id"`
	Type                string    `json:"type"`
	Name                string    `json:"name"`
	Enabled             bool      `json:"enabled"`
	Payload             string    `json:"payload"`
	IntervalSeconds     int       `json:"interval_seconds"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	PausedReason        string    `json:"paused_reason,omitempty"`
	NextRunAt           time.Time `json:"next_run_at,omitempty"`
	LastRunAt           time.Time `json:"last_run_at,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type Activity struct {
	ID          int64     `json:"id"`
	Source      string    `json:"source"`
	Type        string    `json:"type"`
	Result      string    `json:"result"`
	EntityType  string    `json:"entity_type,omitempty"`
	EntityID    string    `json:"entity_id,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	RemoteIP    string    `json:"remote_ip,omitempty"`
	APIClientID string    `json:"api_client_id,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}
