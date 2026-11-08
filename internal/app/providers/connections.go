package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
)

// ConnectionService 统一维护后台任务与管理端操作使用的连接会话。
type ConnectionService struct {
	store   *repository.Store
	hub     *events.Hub
	refresh func(context.Context, string, string) (string, time.Time, error)
	mu      sync.Mutex
	gates   map[string]chan struct{}
}

func (s *ConnectionService) ListPrivacyMailboxes(ctx context.Context, accountID string) ([]protocol.ICloudRemoteMailbox, error) {
	var remote []protocol.ICloudRemoteMailbox
	err := s.useSession(ctx, accountID, domain.ConnectionICloudWeb, func(session protocol.ICloudSession) (protocol.ICloudSession, error) {
		var updated protocol.ICloudSession
		var err error
		remote, updated, err = protocol.NewICloudClient().ListPrivacyMailboxesWithSession(ctx, session)
		return updated, err
	})
	return remote, err
}

func newConnectionService(store *repository.Store, hub *events.Hub) *ConnectionService {
	return &ConnectionService{store: store, hub: hub, refresh: protocol.RefreshConnection, gates: make(map[string]chan struct{})}
}

// 锁覆盖读取、远端请求和保存，避免排队的请求继续使用旧会话。
func (s *ConnectionService) acquire(ctx context.Context, accountID, kind string) (func(), error) {
	key := accountID + ":" + kind
	s.mu.Lock()
	gate := s.gates[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		s.gates[key] = gate
	}
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *ConnectionService) checkJob(ctx context.Context, job domain.Job) error {
	var payload struct {
		AccountID string `json:"account_id"`
		Kind      string `json:"kind"`
		Automatic bool   `json:"automatic"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("连接检测参数无效")
	}
	if payload.AccountID != "" && payload.Kind != "" {
		return s.check(ctx, payload.AccountID, payload.Kind, payload.Automatic)
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, account := range accounts {
		if payload.AccountID != "" && payload.AccountID != account.ID {
			continue
		}
		for _, conn := range account.Connections {
			if payload.Kind != "" && payload.Kind != conn.Kind {
				continue
			}
			if err := s.check(ctx, account.ID, conn.Kind, payload.Automatic); err != nil {
				failures = append(failures, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return errors.Join(failures...)
}

func (s *ConnectionService) keepAliveJob(ctx context.Context, job domain.Job) error {
	var payload struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("保活参数无效")
	}
	encoded, _ := json.Marshal(map[string]any{"account_id": payload.AccountID, "kind": domain.ConnectionAppleAccount, "automatic": true})
	job.Payload = string(encoded)
	return s.checkJob(ctx, job)
}

func (s *ConnectionService) check(ctx context.Context, accountID, kind string, automatic bool) error {
	release, err := s.acquire(ctx, accountID, kind)
	if err != nil {
		return err
	}
	defer release()
	before, err := s.store.ConnectionSecret(ctx, accountID, kind)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if before.Status == domain.ConnectionDisabled {
		return nil
	}
	if connectionCoolingDown(before, time.Now()) {
		return nil
	}
	if automatic && (before.Status == domain.ConnectionExpired || before.Status == domain.ConnectionPending2FA || before.NextCheckAt.After(time.Now())) {
		return nil
	}
	secret := before.SecretJSON
	if kind != domain.ConnectionIMAP {
		var session protocol.ICloudSession
		if json.Unmarshal([]byte(secret), &session) == nil {
			session.AccountID = accountID
			encoded, _ := json.Marshal(session)
			secret = string(encoded)
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	updated, expires, refreshErr := s.refresh(requestCtx, kind, secret)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	after := connectionCheckResult(before, updated, expires, refreshErr, time.Now().UTC())
	saved, err := s.store.SaveConnectionCheck(ctx, before, after)
	if err != nil {
		return err
	}
	if !saved {
		return nil
	}
	s.hub.Publish("connection.changed", map[string]any{"account_id": accountID, "kind": kind, "status": after.Status})
	return refreshErr
}

func connectionCheckResult(before domain.Connection, secret string, expires time.Time, err error, now time.Time) domain.Connection {
	after := before
	if secret != "" {
		after.SecretJSON = secret
		var session protocol.ICloudSession
		if before.Kind != domain.ConnectionIMAP && json.Unmarshal([]byte(secret), &session) == nil && session.Host != "" {
			after.Host = session.Host
		}
	}
	if err == nil {
		after.SecretJSON, after.ExpiresAt, after.ConsecutiveFailures = secret, expires, 0
		after.Status, after.StatusMessage = domain.ConnectionHealthy, "连接正常"
		after.NextCheckAt = nextConnectionCheck(before, expires, now)
		if before.Kind != domain.ConnectionIMAP {
			after.StatusMessage = "连接正常，自动维护已启用"
		}
		return after
	}
	after.ConsecutiveFailures++
	failure := protocol.ClassifySessionError(err)
	if failure.RequiresLogin {
		after.Status = domain.ConnectionExpired
		after.StatusMessage = "授权已失效，请重新登录；自动维护已暂停"
		after.NextCheckAt = time.Time{}
		return after
	}
	after.Status = domain.ConnectionFailed
	after.StatusMessage = "将自动重试；" + connectionFailureMessage(err)
	if failure.RateLimited {
		after.Status = domain.ConnectionRateLimited
		after.StatusMessage = "Apple 请求受限，将按等待时间自动重试"
	}
	delay := time.Minute * time.Duration(1<<min(after.ConsecutiveFailures-1, 6))
	delay = min(delay, time.Hour)
	delay = max(delay, failure.RetryAfter)
	after.NextCheckAt = now.Add(delay)
	if before.Kind == domain.ConnectionIMAP {
		after.NextCheckAt = time.Time{}
		after.StatusMessage = connectionFailureMessage(err)
	}
	return after
}

func connectionCoolingDown(connection domain.Connection, now time.Time) bool {
	return (connection.Status == domain.ConnectionFailed || connection.Status == domain.ConnectionRateLimited) && connection.NextCheckAt.After(now)
}

func nextConnectionCheck(connection domain.Connection, expires, now time.Time) time.Time {
	if connection.Kind == domain.ConnectionIMAP {
		return time.Time{}
	}
	interval := 30 * time.Minute
	if connection.Kind == domain.ConnectionAppleAccount {
		interval = 4 * time.Minute
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(connection.ID))
	interval -= time.Duration(h.Sum32()%uint32(interval/time.Second/10)) * time.Second
	next := now.Add(interval)
	if expires.After(now) && expires.Add(-time.Minute).Before(next) {
		next = expires.Add(-time.Minute)
	}
	if next.Before(now.Add(30 * time.Second)) {
		next = now.Add(30 * time.Second)
	}
	return next
}

// 业务操作与维护共用连接锁，并保存业务响应轮换的凭据。
func (s *ConnectionService) useSession(ctx context.Context, accountID, kind string, operation func(protocol.ICloudSession) (protocol.ICloudSession, error)) error {
	release, err := s.acquire(ctx, accountID, kind)
	if err != nil {
		return err
	}
	defer release()
	before, err := s.store.ConnectionSecret(ctx, accountID, kind)
	if err != nil {
		return err
	}
	if before.Status == domain.ConnectionDisabled {
		return fmt.Errorf("连接已停用，请先启用后再使用")
	}
	if before.Status == domain.ConnectionExpired || before.Status == domain.ConnectionPending2FA || before.Status == domain.ConnectionNotConfigured {
		return fmt.Errorf("连接需要配置或重新登录")
	}
	if connectionCoolingDown(before, time.Now()) {
		return fmt.Errorf("连接正在等待重试，请在 %s 后再试", before.NextCheckAt.UTC().Format(time.RFC3339))
	}
	if before.Status != domain.ConnectionHealthy && before.Status != domain.ConnectionExpiring {
		return fmt.Errorf("连接尚未通过检测，请先检测连接或更新授权")
	}
	var session protocol.ICloudSession
	if json.Unmarshal([]byte(before.SecretJSON), &session) != nil {
		return fmt.Errorf("连接凭据格式无效")
	}
	session.AccountID = accountID
	updated, operationErr := operation(session)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	after := before
	after.SecretJSON = string(encoded)
	after.ExpiresAt = protocol.SessionExpiresAt(kind, updated)
	failure := protocol.ClassifySessionError(operationErr)
	if failure.RequiresLogin {
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		secret, expiry, refreshErr := s.refresh(requestCtx, kind, string(encoded))
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		after = connectionCheckResult(after, secret, expiry, refreshErr, time.Now().UTC())
		if refreshErr == nil {
			operationErr = fmt.Errorf("连接授权已自动恢复，请重新执行本次操作")
		} else {
			operationErr = refreshErr
		}
	} else if operationErr == nil || failure.Temporary || failure.RateLimited || failure.RetryAfter > 0 {
		after = connectionCheckResult(after, string(encoded), after.ExpiresAt, operationErr, time.Now().UTC())
	}
	saved, err := s.store.SaveConnectionCheck(ctx, before, after)
	if err != nil {
		return err
	}
	if saved {
		s.hub.Publish("connection.changed", map[string]any{"account_id": accountID, "kind": kind, "status": after.Status})
	}
	return operationErr
}

func (s *ConnectionService) createRemoteMailbox(ctx context.Context, accountID, strategy, label, note string) (protocol.ICloudRemoteMailbox, error) {
	kinds := []string{domain.ConnectionICloudWeb, domain.ConnectionAppleAccount}
	if strategy != "" && strategy != "auto" {
		if strategy != domain.ConnectionAppleAccount && strategy != domain.ConnectionICloudWeb {
			return protocol.ICloudRemoteMailbox{}, fmt.Errorf("无效创建策略")
		}
		kinds = []string{strategy}
	}
	var remote protocol.ICloudRemoteMailbox
	var last error
	for _, kind := range kinds {
		attempted := false
		last = s.useSession(ctx, accountID, kind, func(session protocol.ICloudSession) (protocol.ICloudSession, error) {
			attempted = true
			client := protocol.NewICloudClient()
			var updated protocol.ICloudSession
			var err error
			if kind == domain.ConnectionAppleAccount {
				remote, updated, err = client.CreatePrivacyMailboxWithAppleAccount(ctx, session, "", label, note)
			} else {
				remote, updated, err = client.CreatePrivacyMailboxWithSession(ctx, session, label, note)
			}
			return updated, err
		})
		if last == nil {
			return remote, nil
		}
		// 请求已经发出时不切换接口重放，避免结果未知时重复创建。
		if attempted {
			return remote, &mailboxCreateError{err: last}
		}
		if ctx.Err() != nil {
			return remote, ctx.Err()
		}
	}
	return remote, last
}
