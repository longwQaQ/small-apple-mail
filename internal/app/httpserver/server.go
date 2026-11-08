package httpserver

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"small-apple-mail/internal/app/config"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
	"small-apple-mail/internal/app/web"
	"sync"
	"time"
)

const sessionCookie = "sam_session"

var validScheduleTypes = map[string]bool{
	"account.connection.check":  true,
	"account.keepalive":         true,
	"mailbox.create":            true,
	"mailbox.remote_sync":       true,
	"mailbox.message_sync":      true,
	"mailbox.remote_clean":      true,
	"message.retention_cleanup": true,
	"database.integrity_check":  true,
}

type Server struct {
	cfg            config.Config
	store          *repository.Store
	hub            *events.Hub
	logger         *slog.Logger
	mux            *http.ServeMux
	failMu         sync.Mutex
	failures       map[string][]time.Time
	protocols      *protocol.Broker
	previewMu      sync.Mutex
	syncPreviews   map[string]syncPreview
	cleanPreviews  map[string]syncPreview
	deletePreviews map[string]accountDeletePreview
	trustedProxies []*net.IPNet
	jobController  JobController
	mailboxReader  MailboxReader
}

type MailboxReader interface {
	ListPrivacyMailboxes(context.Context, string) ([]protocol.ICloudRemoteMailbox, error)
}

type JobController interface {
	Ready() bool
	CancelJob(context.Context, string) error
	ClearJobs(context.Context) (int64, error)
}

type syncPreview struct {
	AccountIDs []string
	ExpiresAt  time.Time
}

type accountDeletePreview struct {
	AccountID string
	ExpiresAt time.Time
}

type ctxKey string

const requestIDKey ctxKey = "request_id"

func New(cfg config.Config, store *repository.Store, hub *events.Hub, logger *slog.Logger, jobs JobController, mailboxes MailboxReader) *Server {
	s := &Server{cfg: cfg, store: store, hub: hub, logger: logger, jobController: jobs, mailboxReader: mailboxes, mux: http.NewServeMux(), failures: map[string][]time.Time{}, protocols: protocol.NewBroker(), syncPreviews: map[string]syncPreview{}, cleanPreviews: map[string]syncPreview{}, deletePreviews: map[string]accountDeletePreview{}}
	for _, value := range cfg.TrustedProxyCIDRs {
		_, network, _ := net.ParseCIDR(value)
		if network != nil {
			s.trustedProxies = append(s.trustedProxies, network)
		}
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID, _ := security.ID("req")
	ctx := context.WithValue(r.Context(), requestIDKey, requestID)
	r = r.WithContext(ctx)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.pageApp)
	s.mux.HandleFunc("GET /login", s.pageLogin)
	s.mux.HandleFunc("GET /assets/{name}", s.asset)
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("POST /api/admin/session", s.login)
	s.mux.HandleFunc("GET /api/admin/session", s.session)
	s.mux.HandleFunc("DELETE /api/admin/session", s.logout)
	s.mux.HandleFunc("PUT /api/admin/password", s.changePassword)
	s.mux.HandleFunc("GET /api/admin/dashboard", s.dashboard)
	s.mux.HandleFunc("GET /api/admin/accounts", s.accounts)
	s.mux.HandleFunc("POST /api/admin/accounts", s.accounts)
	s.mux.HandleFunc("GET /api/admin/accounts/{account_id}", s.accountDetail)
	s.mux.HandleFunc("PATCH /api/admin/accounts/{account_id}", s.accountDetail)
	s.mux.HandleFunc("DELETE /api/admin/accounts/{account_id}", s.accountDetail)
	s.mux.HandleFunc("GET /api/admin/accounts/{account_id}/delete-preview", s.accountDeletePreview)
	s.mux.HandleFunc("GET /api/admin/accounts/{account_id}/connections", s.accountConnections)
	s.mux.HandleFunc("PUT /api/admin/accounts/{account_id}/connections/{kind}", s.saveConnection)
	s.mux.HandleFunc("POST /api/admin/accounts/{account_id}/connections/{kind}/verify", s.verifyConnection)
	s.mux.HandleFunc("POST /api/admin/accounts/{account_id}/connections/{kind}/check", s.checkConnection)
	s.mux.HandleFunc("DELETE /api/admin/accounts/{account_id}/connections/{kind}", s.deleteConnection)
	s.mux.HandleFunc("PATCH /api/admin/accounts/{account_id}/connections/{kind}/enabled", s.setOptionalConnectionEnabled)
	s.mux.HandleFunc("GET /api/admin/mailboxes", s.mailboxes)
	s.mux.HandleFunc("POST /api/admin/mailboxes", s.mailboxes)
	s.mux.HandleFunc("GET /api/admin/mailbox-overview", s.mailboxOverview)
	s.mux.HandleFunc("POST /api/admin/mailboxes/batch", s.batchMailboxes)
	s.mux.HandleFunc("GET /api/admin/mailboxes/{mailbox_id}/result", s.mailboxResult)
	s.mux.HandleFunc("POST /api/admin/mailbox-sync-previews", s.mailboxSyncPreview)
	s.mux.HandleFunc("POST /api/admin/mailbox-sync-jobs", s.mailboxSyncJob)
	s.mux.HandleFunc("POST /api/admin/mailbox-clean-previews", s.mailboxCleanPreview)
	s.mux.HandleFunc("POST /api/admin/mailbox-clean-jobs", s.mailboxCleanJob)
	s.mux.HandleFunc("GET /api/admin/mailboxes/{mailbox_id}", s.mailboxDetail)
	s.mux.HandleFunc("PATCH /api/admin/mailboxes/{mailbox_id}", s.mailboxDetail)
	s.mux.HandleFunc("DELETE /api/admin/mailboxes/{mailbox_id}", s.mailboxDetail)
	s.mux.HandleFunc("GET /api/admin/mailboxes/{mailbox_id}/messages", s.mailboxMessages)
	s.mux.HandleFunc("GET /api/admin/mailboxes/{mailbox_id}/messages/{message_id}", s.mailboxMessage)
	s.mux.HandleFunc("POST /api/admin/mailboxes/{mailbox_id}/message-sync-jobs", s.mailboxMessageSync)
	s.mux.HandleFunc("POST /api/admin/mailboxes/{mailbox_id}/api-credential", s.rotateMailboxAPICredential)
	s.mux.HandleFunc("GET /api/admin/messages", s.inbox)
	s.mux.HandleFunc("GET /api/admin/jobs", s.jobs)
	s.mux.HandleFunc("POST /api/admin/jobs", s.jobs)
	s.mux.HandleFunc("DELETE /api/admin/jobs", s.jobs)
	s.mux.HandleFunc("GET /api/admin/jobs/{job_id}", s.jobDetail)
	s.mux.HandleFunc("POST /api/admin/jobs/{job_id}/cancel", s.cancelJob)
	s.mux.HandleFunc("GET /api/admin/jobs/{job_id}/events", s.jobEvents)
	s.mux.HandleFunc("GET /api/admin/jobs/{job_id}/items", s.jobItems)
	s.mux.HandleFunc("POST /api/admin/jobs/{job_id}/retry", s.retryJob)
	s.mux.HandleFunc("GET /api/admin/schedules", s.schedules)
	s.mux.HandleFunc("POST /api/admin/schedules", s.schedules)
	s.mux.HandleFunc("GET /api/admin/schedules/{schedule_id}", s.scheduleDetail)
	s.mux.HandleFunc("PATCH /api/admin/schedules/{schedule_id}", s.scheduleDetail)
	s.mux.HandleFunc("DELETE /api/admin/schedules/{schedule_id}", s.scheduleDetail)
	s.mux.HandleFunc("POST /api/admin/schedules/{schedule_id}/run", s.runSchedule)
	s.mux.HandleFunc("GET /api/admin/api-clients", s.apiClients)
	s.mux.HandleFunc("POST /api/admin/api-clients", s.apiClients)
	s.mux.HandleFunc("PATCH /api/admin/api-clients/{client_id}", s.apiClientDetail)
	s.mux.HandleFunc("DELETE /api/admin/api-clients/{client_id}", s.apiClientDetail)
	s.mux.HandleFunc("POST /api/admin/api-clients/{client_id}/rotate", s.rotateAPIClient)
	s.mux.HandleFunc("GET /api/admin/settings", s.settings)
	s.mux.HandleFunc("PUT /api/admin/settings", s.settings)
	s.mux.HandleFunc("GET /api/admin/activities", s.activity)
	s.mux.HandleFunc("POST /api/admin/activities", s.activity)
	s.mux.HandleFunc("PATCH /api/admin/activities/{activity_id}", s.activityDetail)
	s.mux.HandleFunc("DELETE /api/admin/activities/{activity_id}", s.activityDetail)
	s.mux.HandleFunc("GET /api/admin/events", s.eventStream)
	s.mux.HandleFunc("GET /api/openapi.json", s.openapi)
	s.mux.HandleFunc("GET /api/mailboxes/{mailbox_id}/code", s.mailboxAPICode)
	s.mux.HandleFunc("POST /api/leases", s.createLease)
	s.mux.HandleFunc("GET /api/leases/{lease_id}", s.getLease)
	s.mux.HandleFunc("POST /api/leases/{lease_id}/renew", s.renewLease)
	s.mux.HandleFunc("DELETE /api/leases/{lease_id}", s.releaseLease)
	s.mux.HandleFunc("GET /api/leases/{lease_id}/code", s.leaseCode)
}

func requestID(r *http.Request) string { v, _ := r.Context().Value(requestIDKey).(string); return v }

var _ fs.FS = web.FS
