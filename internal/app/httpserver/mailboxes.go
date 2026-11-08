package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
	"strconv"
	"strings"
	"time"
)

func (s *Server) mailboxes(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, next, err := s.store.FilterMailboxes(r.Context(), repository.MailboxFilter{Query: r.URL.Query().Get("query"), Status: r.URL.Query().Get("status"), AccountID: r.URL.Query().Get("account_id"), Cursor: r.URL.Query().Get("cursor"), Limit: limit, Mode: r.URL.Query().Get("allocation_mode"), Tag: r.URL.Query().Get("tag"), WithResult: r.URL.Query().Get("with_result") == "true", Unused: r.URL.Query().Get("unused") == "true"})
		if err != nil {
			writeErr(w, r, 500, "mailboxes_failed", "无法读取邮箱", true, nil)
			return
		}
		writeData(w, r, 200, map[string]any{"items": rows, "next_cursor": next}, nil)
		return
	}
	var body struct {
		AccountID      string   `json:"account_id"`
		Email          string   `json:"email"`
		Label          string   `json:"label"`
		Note           string   `json:"note"`
		Tags           []string `json:"tags"`
		AllocationMode string   `json:"allocation_mode"`
		Purpose        string   `json:"purpose"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	body.AccountID = strings.TrimSpace(body.AccountID)
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	if body.AccountID == "" {
		writeErr(w, r, 400, "account_required", "请选择 Apple 账号", false, nil)
		return
	}
	address, addressErr := mail.ParseAddress(body.Email)
	if addressErr != nil || !strings.EqualFold(address.Address, body.Email) || !strings.Contains(body.Email, "@") {
		writeErr(w, r, 400, "invalid_email", "请输入有效的邮箱地址", false, nil)
		return
	}
	if _, err := s.store.Account(r.Context(), body.AccountID); err != nil {
		writeErr(w, r, 404, "account_not_found", "Apple 账号不存在", false, nil)
		return
	}
	m, err := s.store.CreateMailbox(r.Context(), body.AccountID, body.Email, body.Label, body.Note, body.Tags, repository.MailboxPatch{AllocationMode: &body.AllocationMode, Purpose: &body.Purpose})
	if err != nil {
		writeErr(w, r, 409, "mailbox_create_failed", err.Error(), false, nil)
		return
	}
	s.hub.Publish("mailbox.changed", m)
	writeData(w, r, 201, m, nil)
}

func (s *Server) mailboxSyncPreview(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, r, 500, "accounts_failed", "无法读取账号", true, nil)
		return
	}
	wanted := map[string]bool{}
	for _, id := range body.AccountIDs {
		wanted[id] = true
	}
	summary := map[string]int{"remote": 0, "local": 0, "created": 0, "missing": 0, "accounts": 0}
	selected := []string{}
	for _, account := range accounts {
		if len(wanted) > 0 && !wanted[account.ID] {
			continue
		}
		_, connErr := s.store.ConnectionSecret(r.Context(), account.ID, domain.ConnectionICloudWeb)
		if errors.Is(connErr, repository.ErrNotFound) {
			continue
		}
		if connErr != nil || s.mailboxReader == nil {
			writeErr(w, r, 503, "sync_preview_unavailable", "同步预览暂不可用", true, nil)
			return
		}
		remote, remoteErr := s.mailboxReader.ListPrivacyMailboxes(r.Context(), account.ID)
		if remoteErr != nil {
			writeErr(w, r, 502, "remote_sync_preview_failed", "账号 "+account.AppleID+" 预览失败: "+remoteErr.Error(), true, nil)
			return
		}
		local, localErr := s.store.AllMailboxesByAccount(r.Context(), account.ID)
		if localErr != nil {
			writeErr(w, r, 500, "mailboxes_failed", "无法读取本地邮箱", true, nil)
			return
		}
		remoteSet := map[string]bool{}
		localSet := map[string]bool{}
		for _, m := range remote {
			remoteSet[strings.ToLower(m.Email)] = true
		}
		for _, m := range local {
			localSet[strings.ToLower(m.Email)] = true
		}
		for email := range remoteSet {
			if !localSet[email] {
				summary["created"]++
			}
		}
		for email := range localSet {
			if !remoteSet[email] {
				summary["missing"]++
			}
		}
		summary["remote"] += len(remote)
		summary["local"] += len(local)
		summary["accounts"]++
		selected = append(selected, account.ID)
	}
	if len(selected) == 0 {
		writeErr(w, r, 400, "icloud_web_connection_required", "没有可用于同步的 iCloud 邮箱管理连接", false, nil)
		return
	}
	token, _ := security.RandomToken("sync_", 24)
	s.previewMu.Lock()
	now := time.Now().UTC()
	for key, value := range s.syncPreviews {
		if value.ExpiresAt.Before(now) {
			delete(s.syncPreviews, key)
		}
	}
	s.syncPreviews[token] = syncPreview{AccountIDs: selected, ExpiresAt: now.Add(10 * time.Minute)}
	s.previewMu.Unlock()
	writeData(w, r, 200, map[string]any{"confirmation_token": token, "expires_at": now.Add(10 * time.Minute), "summary": summary}, nil)
}

func (s *Server) mailboxSyncJob(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	s.previewMu.Lock()
	preview, exists := s.syncPreviews[body.ConfirmationToken]
	if exists {
		delete(s.syncPreviews, body.ConfirmationToken)
	}
	s.previewMu.Unlock()
	if !exists || preview.ExpiresAt.Before(time.Now().UTC()) {
		writeErr(w, r, 409, "sync_preview_expired", "同步预览已过期，请重新预览", false, nil)
		return
	}
	payload, _ := json.Marshal(map[string]any{"account_ids": preview.AccountIDs})
	job, err := s.store.CreateJob(r.Context(), "mailbox.remote_sync", string(payload), 10)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建同步任务", true, nil)
		return
	}
	s.hub.Publish("job.created", job)
	writeData(w, r, 202, job, nil)
}

func (s *Server) mailboxCleanPreview(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		AccountIDs []string `json:"account_ids"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, r, 500, "accounts_failed", "无法读取账号", true, nil)
		return
	}
	wanted := map[string]bool{}
	for _, id := range body.AccountIDs {
		wanted[id] = true
	}
	selected := []string{}
	messages := 0
	for _, account := range accounts {
		if len(wanted) > 0 && !wanted[account.ID] {
			continue
		}
		if _, connErr := s.store.ConnectionSecret(r.Context(), account.ID, domain.ConnectionIMAP); connErr != nil {
			continue
		}
		ids, idErr := s.store.RemoteVerificationMessageIDs(r.Context(), account.ID)
		if idErr != nil {
			writeErr(w, r, 500, "messages_failed", "无法读取远端消息标识", true, nil)
			return
		}
		if len(ids) > 0 {
			selected = append(selected, account.ID)
			messages += len(ids)
		}
	}
	if len(selected) == 0 {
		writeErr(w, r, 400, "nothing_to_clean", "没有可清理的已同步远端消息", false, nil)
		return
	}
	token, _ := security.RandomToken("clean_", 24)
	now := time.Now().UTC()
	s.previewMu.Lock()
	s.cleanPreviews[token] = syncPreview{AccountIDs: selected, ExpiresAt: now.Add(10 * time.Minute)}
	s.previewMu.Unlock()
	writeData(w, r, 200, map[string]any{"confirmation_token": token, "expires_at": now.Add(10 * time.Minute), "accounts": len(selected), "messages": messages}, nil)
}

func (s *Server) mailboxCleanJob(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	s.previewMu.Lock()
	preview, exists := s.cleanPreviews[body.ConfirmationToken]
	if exists {
		delete(s.cleanPreviews, body.ConfirmationToken)
	}
	s.previewMu.Unlock()
	if !exists || preview.ExpiresAt.Before(time.Now().UTC()) {
		writeErr(w, r, 409, "clean_preview_expired", "清理预览已过期，请重新预览", false, nil)
		return
	}
	payload, _ := json.Marshal(map[string]any{"account_ids": preview.AccountIDs})
	job, err := s.store.CreateJob(r.Context(), "mailbox.remote_clean", string(payload), 20)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建远端清理任务", true, nil)
		return
	}
	s.hub.Publish("job.created", job)
	writeData(w, r, 202, job, nil)
}

func (s *Server) mailboxDetail(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	id := r.PathValue("mailbox_id")
	if r.Method == "GET" {
		mailbox, err := s.store.Mailbox(r.Context(), id)
		if err != nil {
			writeErr(w, r, 404, "mailbox_not_found", "邮箱不存在", false, nil)
			return
		}
		writeData(w, r, 200, mailbox, nil)
		return
	}
	if r.Method == "DELETE" {
		if err := s.store.DeleteMailbox(r.Context(), id); err != nil {
			writeErr(w, r, 409, "mailbox_delete_failed", err.Error(), false, nil)
			return
		}
		w.WriteHeader(204)
		return
	}
	var body repository.MailboxPatch
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	mailbox, err := s.store.PatchMailbox(r.Context(), id, body)
	if err != nil {
		writeErr(w, r, 400, "mailbox_update_failed", err.Error(), false, nil)
		return
	}
	s.hub.Publish("mailbox.changed", mailbox)
	writeData(w, r, 200, mailbox, nil)
}

func (s *Server) mailboxMessages(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 10
	}
	rows, total, err := s.store.ListMessagesPage(r.Context(), r.PathValue("mailbox_id"), page, pageSize)
	if err != nil {
		writeErr(w, r, 500, "messages_failed", "无法读取收件列表", true, nil)
		return
	}
	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	writeData(w, r, 200, map[string]any{"items": rows, "total": total, "page": page, "page_size": pageSize, "pages": pages}, nil)
}

func (s *Server) mailboxMessage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	message, err := s.store.Message(r.Context(), r.PathValue("mailbox_id"), r.PathValue("message_id"))
	if errors.Is(err, repository.ErrNotFound) {
		writeErr(w, r, 404, "message_not_found", "邮件不存在", false, nil)
		return
	}
	if err != nil {
		writeErr(w, r, 500, "message_failed", "无法读取邮件正文", true, nil)
		return
	}
	writeData(w, r, 200, message, nil)
}

func (s *Server) mailboxMessageSync(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	mailbox, err := s.store.Mailbox(r.Context(), r.PathValue("mailbox_id"))
	if err != nil {
		writeErr(w, r, 404, "mailbox_not_found", "邮箱不存在", false, nil)
		return
	}
	payload, _ := json.Marshal(map[string]any{"account_id": mailbox.AccountID})
	job, err := s.store.CreateJob(r.Context(), "mailbox.message_sync", string(payload), 15)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建消息同步任务", true, nil)
		return
	}
	s.hub.Publish("job.created", job)
	writeData(w, r, 202, job, nil)
}

func (s *Server) rotateMailboxAPICredential(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	mailboxID := r.PathValue("mailbox_id")
	token, err := s.store.RotateMailboxAPIToken(r.Context(), mailboxID)
	if err != nil {
		writeErr(w, r, 404, "mailbox_not_found", "邮箱不存在", false, nil)
		return
	}
	base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	apiURL := fmt.Sprintf("%s/api/mailboxes/%s/code?key=%s&wait_seconds=12", base, url.PathEscape(mailboxID), url.QueryEscape(token))
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "mailbox.api_credential.rotated", Result: "success", EntityType: "mailbox", EntityID: mailboxID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "邮箱取码 API 凭据已重新生成"})
	writeData(w, r, http.StatusCreated, map[string]any{"api_url": apiURL, "rotated": true}, nil)
}

func (s *Server) mailboxAPICode(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("key"))
	if token == "" {
		token = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	mailbox, err := s.store.MailboxByAPIToken(r.Context(), r.PathValue("mailbox_id"), token)
	if err != nil {
		writeErr(w, r, http.StatusUnauthorized, "mailbox_api_invalid", "邮箱 API 凭据无效或已轮换", false, nil)
		return
	}
	after := time.Now().UTC().Add(-24 * time.Hour)
	if value := strings.TrimSpace(r.URL.Query().Get("after")); value != "" {
		parsed, parseErr := time.Parse(time.RFC3339, value)
		if parseErr != nil {
			writeErr(w, r, http.StatusBadRequest, "invalid_after", "after 必须是有效的 RFC 3339 时间", false, nil)
			return
		}
		after = parsed
	}
	wait, parseErr := strconv.Atoi(r.URL.Query().Get("wait_seconds"))
	if r.URL.Query().Get("wait_seconds") == "" {
		wait = 0
		parseErr = nil
	}
	if parseErr != nil || wait < 0 || wait > 30 {
		writeErr(w, r, http.StatusBadRequest, "invalid_wait", "wait_seconds 必须在 0-30 之间", false, nil)
		return
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	syncTriggered := false
	for {
		if syncTriggered {
			mailbox, err = s.store.MailboxByAPIToken(r.Context(), r.PathValue("mailbox_id"), token)
			if err != nil {
				writeErr(w, r, http.StatusUnauthorized, "mailbox_api_invalid", "邮箱 API 凭据无效或已轮换", false, nil)
				return
			}
		}
		message, lookupErr := s.store.FindMailboxCode(r.Context(), mailbox.ID, after, r.URL.Query().Get("keyword"))
		if lookupErr == nil {
			writeData(w, r, http.StatusOK, map[string]any{"email": mailbox.Email, "code": message.Code, "sender": message.Sender, "subject": message.Subject, "received_at": message.ReceivedAt, "message_id": message.ID}, nil)
			return
		}
		if !errors.Is(lookupErr, repository.ErrNotFound) {
			writeErr(w, r, http.StatusInternalServerError, "code_lookup_failed", "无法查询验证码", true, nil)
			return
		}
		if !syncTriggered {
			job, created, jobErr := s.store.EnsureMessageSyncJob(r.Context(), mailbox.AccountID, 15)
			if jobErr != nil {
				writeErr(w, r, http.StatusInternalServerError, "message_sync_failed", "无法触发邮件同步", true, nil)
				return
			}
			if created {
				s.hub.Publish("job.created", job)
			}
			syncTriggered = true
		}
		if !time.Now().Before(deadline) {
			writeErr(w, r, http.StatusNotFound, "code_not_found", "等待时间内没有符合条件的验证码", true, nil)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
