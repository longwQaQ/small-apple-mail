package providers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
)

type imapWatch struct {
	signature string
	cancel    context.CancelFunc
}
type IMAPManager struct {
	store   *repository.Store
	hub     *events.Hub
	mu      sync.Mutex
	watches map[string]imapWatch
}

func NewIMAPManager(store *repository.Store, hub *events.Hub) *IMAPManager {
	return &IMAPManager{store: store, hub: hub, watches: map[string]imapWatch{}}
}
func (m *IMAPManager) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		m.reconcile(ctx)
		for {
			select {
			case <-ctx.Done():
				m.stopAll()
				return
			case <-ticker.C:
				m.reconcile(ctx)
			}
		}
	}()
}
func (m *IMAPManager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, w := range m.watches {
		w.cancel()
		delete(m.watches, id)
	}
}
func (m *IMAPManager) reconcile(parent context.Context) {
	accounts, err := m.store.ListAccounts(parent)
	if err != nil {
		return
	}
	active := map[string]bool{}
	for _, account := range accounts {
		connection, err := m.store.ConnectionSecret(parent, account.ID, domain.ConnectionIMAP)
		if err != nil || connection.Status == domain.ConnectionDisabled {
			continue
		}
		active[account.ID] = true
		signature := security.HashToken(connection.SecretJSON)
		m.mu.Lock()
		current, exists := m.watches[account.ID]
		if exists && current.signature == signature {
			m.mu.Unlock()
			continue
		}
		if exists {
			current.cancel()
		}
		ctx, cancel := context.WithCancel(parent)
		m.watches[account.ID] = imapWatch{signature: signature, cancel: cancel}
		m.mu.Unlock()
		go m.watch(ctx, account.ID, connection.SecretJSON)
	}
	m.mu.Lock()
	for id, w := range m.watches {
		if !active[id] {
			w.cancel()
			delete(m.watches, id)
		}
	}
	m.mu.Unlock()
}
func (m *IMAPManager) watch(ctx context.Context, accountID, secretJSON string) {
	var secret struct {
		Email       string `json:"email"`
		AppPassword string `json:"app_password"`
	}
	if json.Unmarshal([]byte(secretJSON), &secret) != nil {
		return
	}
	state := protocol.LoginState{Kind: protocol.LoginStateICloudIMAP, IMAPEmail: secret.Email, IMAPUsername: secret.Email, IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPAppPassword: secret.AppPassword}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		ready := false
		err := protocol.WatchICloudIMAPExistsWithReady(ctx, state, func() {
			ready = true
			backoff = time.Second
			if saved, _ := m.store.SetIMAPWatchStatus(ctx, accountID, secretJSON, domain.ConnectionHealthy, "IMAP 实时监听正常"); saved {
				m.hub.Publish("connection.changed", map[string]any{"account_id": accountID, "kind": domain.ConnectionIMAP, "status": domain.ConnectionHealthy})
				m.enqueueMessageSync(ctx, accountID)
			}
		}, func() {
			m.enqueueMessageSync(ctx, accountID)
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second
		} else {
			if saved, _ := m.store.SetIMAPWatchStatus(ctx, accountID, secretJSON, domain.ConnectionFailed, imapWatchFailureMessage(err)); saved {
				m.hub.Publish("connection.changed", map[string]any{"account_id": accountID, "kind": domain.ConnectionIMAP, "status": domain.ConnectionFailed})
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !ready && backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (m *IMAPManager) enqueueMessageSync(ctx context.Context, accountID string) {
	if job, created, err := m.store.RequestMessageSyncJob(ctx, accountID); err == nil && created {
		m.hub.Publish("job.created", job)
	}
}

func imapWatchFailureMessage(err error) string {
	const prefix = "IMAP 实时监听中断，正在重连"
	if err == nil || strings.TrimSpace(err.Error()) == "" {
		return prefix
	}
	message := []rune(prefix + "：" + strings.TrimSpace(err.Error()))
	if len(message) <= 300 {
		return string(message)
	}
	return string(message[:299]) + "…"
}
