package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/web"
	"strconv"
	"strings"
	"time"
)

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, r, 500, "sse_unavailable", "服务器不支持实时事件", false, nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	ch, unsubscribe := s.hub.Subscribe(after)
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case e := <-ch:
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, e.Data)
			flusher.Flush()
		}
	}
}

func bearer(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return ""
}

func (s *Server) publicClient(w http.ResponseWriter, r *http.Request, scope string) (domain.APIClient, bool) {
	key := bearer(r)
	client, err := s.store.APIClientByKey(r.Context(), key)
	if err != nil || client.Status != "active" {
		writeErr(w, r, 401, "invalid_api_key", "API Key 无效或已停用", false, nil)
		return client, false
	}
	if !repository.HasScope(client, scope) {
		writeErr(w, r, 403, "insufficient_scope", "API Key 缺少权限域 "+scope, false, map[string]any{"required_scope": scope})
		return client, false
	}
	return client, true
}

func (s *Server) createLease(w http.ResponseWriter, r *http.Request) {
	client, ok := s.publicClient(w, r, "leases:write")
	if !ok {
		return
	}
	var body repository.LeaseRequest
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	body.IdempotencyKey = r.Header.Get("Idempotency-Key")
	lease, mailbox, token, replay, err := s.store.CreateLease(r.Context(), client, body)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrPoolEmpty):
			writeErr(w, r, 409, "mailbox_pool_empty", "没有符合条件的可用隐私邮箱", true, nil)
		case errors.Is(err, repository.ErrIdempotencyConflict):
			writeErr(w, r, 409, "idempotency_conflict", "相同 Idempotency-Key 对应不同请求", false, nil)
		default:
			writeErr(w, r, 400, "lease_create_failed", err.Error(), false, nil)
		}
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "lease.created", Result: "success", EntityType: "lease", EntityID: lease.ID, APIClientID: client.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "临时邮箱租约已创建"})
	s.hub.Publish("lease.changed", map[string]any{"id": lease.ID, "status": lease.Status})
	status := 201
	if replay {
		status = 200
	}
	writeData(w, r, status, map[string]any{"id": lease.ID, "email": mailbox.Email, "lease_token": token, "status": lease.Status, "expires_at": lease.ExpiresAt}, map[string]any{"idempotent_replay": replay})
}

func (s *Server) leaseCredentials(w http.ResponseWriter, r *http.Request, scope string) (domain.APIClient, domain.Lease, domain.Mailbox, bool) {
	client, ok := s.publicClient(w, r, scope)
	if !ok {
		return client, domain.Lease{}, domain.Mailbox{}, false
	}
	lease, mailbox, err := s.store.LeaseByCredentials(r.Context(), client.ID, r.PathValue("lease_id"), r.Header.Get("X-Lease-Token"))
	if err != nil {
		writeErr(w, r, 404, "lease_inactive", "租约不存在、已过期或凭据错误", false, nil)
		return client, lease, mailbox, false
	}
	return client, lease, mailbox, true
}

func (s *Server) getLease(w http.ResponseWriter, r *http.Request) {
	_, lease, mailbox, ok := s.leaseCredentials(w, r, "leases:read")
	if !ok {
		return
	}
	writeData(w, r, 200, map[string]any{"id": lease.ID, "email": mailbox.Email, "status": lease.Status, "project": lease.Project, "purpose": lease.Purpose, "created_at": lease.CreatedAt, "expires_at": lease.ExpiresAt}, nil)
}

func (s *Server) renewLease(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.leaseCredentials(w, r, "leases:write")
	if !ok {
		return
	}
	var body struct {
		TTL int `json:"ttl_seconds"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	lease, err := s.store.RenewLease(r.Context(), client.ID, r.PathValue("lease_id"), r.Header.Get("X-Lease-Token"), body.TTL)
	if err != nil {
		writeErr(w, r, 409, "lease_renew_failed", "租约无法续期或已达到最长生命周期", false, nil)
		return
	}
	s.hub.Publish("lease.changed", map[string]any{"id": lease.ID, "status": lease.Status})
	writeData(w, r, 200, map[string]any{"id": lease.ID, "status": lease.Status, "expires_at": lease.ExpiresAt}, nil)
}

func (s *Server) releaseLease(w http.ResponseWriter, r *http.Request) {
	client, _, _, ok := s.leaseCredentials(w, r, "leases:write")
	if !ok {
		return
	}
	if err := s.store.ReleaseLease(r.Context(), client.ID, r.PathValue("lease_id"), r.Header.Get("X-Lease-Token"), domain.LeaseReleased); err != nil {
		writeErr(w, r, 404, "lease_inactive", "租约不存在或已失效", false, nil)
		return
	}
	s.hub.Publish("lease.changed", map[string]any{"id": r.PathValue("lease_id"), "status": domain.LeaseReleased})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) leaseCode(w http.ResponseWriter, r *http.Request) {
	_, lease, mailbox, ok := s.leaseCredentials(w, r, "codes:read")
	if !ok {
		return
	}
	after, err := time.Parse(time.RFC3339, r.URL.Query().Get("after"))
	if err != nil {
		writeErr(w, r, 400, "after_required", "after 必须是有效的 RFC 3339 时间", false, nil)
		return
	}
	wait, waitErr := strconv.Atoi(r.URL.Query().Get("wait_seconds"))
	if (waitErr != nil && r.URL.Query().Get("wait_seconds") != "") || wait < 0 || wait > 30 {
		writeErr(w, r, 400, "invalid_wait", "wait_seconds 必须在 0-30 之间", false, nil)
		return
	}
	peek := r.URL.Query().Get("peek") == "true"
	keyword := r.URL.Query().Get("keyword")
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	syncTriggered := false
	for {
		if syncTriggered {
			_, lease, mailbox, ok = s.leaseCredentials(w, r, "codes:read")
			if !ok {
				return
			}
		}
		message, err := s.store.FindCode(r.Context(), lease, after, keyword, peek)
		if err == nil {
			writeData(w, r, 200, map[string]any{"code": message.Code, "sender": message.Sender, "subject": message.Subject, "received_at": message.ReceivedAt, "message_id": message.ID, "consumed": !peek, "synced_for_request": syncTriggered}, nil)
			return
		}
		if errors.Is(err, repository.ErrLeaseInactive) {
			writeErr(w, r, 404, "lease_inactive", "租约不存在或已失效", false, nil)
			return
		}
		if !errors.Is(err, repository.ErrNotFound) {
			writeErr(w, r, 500, "code_lookup_failed", "无法查询验证码", true, nil)
			return
		}
		if !syncTriggered {
			job, created, jobErr := s.store.EnsureMessageSyncJob(r.Context(), mailbox.AccountID, 15)
			if jobErr != nil {
				writeErr(w, r, 500, "message_sync_failed", "无法触发邮件同步", true, nil)
				return
			}
			if created {
				s.hub.Publish("job.created", job)
			}
			syncTriggered = true
		}
		if time.Now().After(deadline) {
			writeErr(w, r, 404, "code_not_found", "等待时间内没有符合条件的验证码", true, nil)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	data, err := web.FS.ReadFile("openapi.json")
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "openapi_unavailable", "OpenAPI 文档不可用", true, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}
