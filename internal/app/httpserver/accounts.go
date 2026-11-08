package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
	"strings"
	"time"
)

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, err := s.store.ListAccounts(r.Context())
		if err != nil {
			writeErr(w, r, 500, "accounts_failed", "无法读取账号", true, nil)
			return
		}
		writeData(w, r, 200, rows, nil)
		return
	}
	var body struct {
		AppleID     string `json:"apple_id"`
		DisplayName string `json:"display_name"`
		Note        string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	a, err := s.store.CreateAccount(r.Context(), body.AppleID, body.DisplayName, body.Note)
	if err != nil {
		writeErr(w, r, 409, "account_create_failed", err.Error(), false, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "account.created", Result: "success", EntityType: "account", EntityID: a.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "Apple 账号已添加"})
	s.hub.Publish("connection.changed", a)
	writeData(w, r, 201, a, nil)
}

func (s *Server) accountDeletePreview(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	impact, err := s.store.AccountImpact(r.Context(), r.PathValue("account_id"))
	if err != nil {
		writeErr(w, r, 404, "account_not_found", "账号不存在", false, nil)
		return
	}
	token, _ := security.RandomToken("delete_", 24)
	now := time.Now().UTC()
	s.previewMu.Lock()
	for key, preview := range s.deletePreviews {
		if preview.ExpiresAt.Before(now) {
			delete(s.deletePreviews, key)
		}
	}
	s.deletePreviews[token] = accountDeletePreview{AccountID: r.PathValue("account_id"), ExpiresAt: now.Add(10 * time.Minute)}
	s.previewMu.Unlock()
	writeData(w, r, 200, map[string]any{"impact": impact, "confirmation_token": token, "expires_at": now.Add(10 * time.Minute)}, nil)
}

func (s *Server) accountDetail(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	id := r.PathValue("account_id")
	if r.Method == "GET" {
		account, err := s.store.Account(r.Context(), id)
		if err != nil {
			writeErr(w, r, 404, "account_not_found", "账号不存在", false, nil)
			return
		}
		writeData(w, r, 200, account, nil)
		return
	}
	if r.Method == "DELETE" {
		var body struct {
			ConfirmationToken string `json:"confirmation_token"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
			return
		}
		s.previewMu.Lock()
		preview, exists := s.deletePreviews[body.ConfirmationToken]
		if exists {
			delete(s.deletePreviews, body.ConfirmationToken)
		}
		s.previewMu.Unlock()
		if !exists || preview.AccountID != id || preview.ExpiresAt.Before(time.Now().UTC()) {
			writeErr(w, r, 409, "account_delete_preview_expired", "删除预览已过期，请重新确认影响", false, nil)
			return
		}
		if err := s.store.DeleteAccount(r.Context(), id); err != nil {
			writeErr(w, r, 409, "account_delete_failed", err.Error(), false, nil)
			return
		}
		_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "account.deleted", Result: "success", EntityType: "account", EntityID: id, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "账号及本地关联数据已删除"})
		w.WriteHeader(204)
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
		Note        string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	account, err := s.store.UpdateAccount(r.Context(), id, body.DisplayName, body.Note)
	if err != nil {
		writeErr(w, r, 404, "account_not_found", "账号不存在", false, nil)
		return
	}
	writeData(w, r, 200, account, nil)
}

func (s *Server) accountConnections(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	rows, err := s.store.ListConnections(r.Context(), r.PathValue("account_id"))
	if err != nil {
		writeErr(w, r, 500, "connections_failed", "无法读取连接", true, nil)
		return
	}
	if rows == nil {
		rows = []domain.Connection{}
	}
	writeData(w, r, 200, rows, nil)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if err := s.store.DeleteConnection(r.Context(), r.PathValue("account_id"), r.PathValue("kind")); err != nil {
		writeErr(w, r, 404, "connection_not_found", "连接不存在", false, nil)
		return
	}
	s.hub.Publish("connection.changed", map[string]any{"account_id": r.PathValue("account_id"), "kind": r.PathValue("kind"), "status": domain.ConnectionNotConfigured})
	w.WriteHeader(204)
}

func (s *Server) saveConnection(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !domain.ValidConnectionKinds[kind] {
		writeErr(w, r, 400, "invalid_connection_kind", "无效连接类型", false, nil)
		return
	}
	var body map[string]any
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	if kind == domain.ConnectionIMAP {
		email, _ := body["email"].(string)
		password, _ := body["app_password"].(string)
		if strings.TrimSpace(email) == "" || strings.TrimSpace(password) == "" {
			writeErr(w, r, 400, "imap_credentials_required", "邮箱和 App 专用密码不能为空", false, nil)
			return
		}
		secretJSON, _ := json.Marshal(map[string]any{"email": strings.TrimSpace(email), "app_password": password})
		conn, err := s.store.UpsertConnection(r.Context(), r.PathValue("account_id"), kind, domain.ConnectionChecking, "imap.mail.me.com", string(secretJSON), "等待后台健康检测")
		if err != nil {
			writeErr(w, r, 500, "connection_save_failed", "无法保存连接", true, nil)
			return
		}
		job, err := s.createConnectionCheckJob(r.Context(), conn.AccountID, kind)
		if err != nil {
			writeErr(w, r, 500, "job_create_failed", "无法创建检测任务", true, nil)
			return
		}
		writeData(w, r, 200, map[string]any{"connection": conn, "job": job}, nil)
		return
	}
	appleID, _ := body["apple_id"].(string)
	password, _ := body["password"].(string)
	method, _ := body["two_factor_method"].(string)
	if appleID == "" || password == "" {
		writeErr(w, r, 400, "apple_credentials_required", "Apple ID 和密码不能为空", false, nil)
		return
	}
	result, err := s.protocols.StartLogin(r.Context(), kind, appleID, password, method)
	password = ""
	if err != nil {
		writeErr(w, r, 502, "apple_login_failed", err.Error(), true, nil)
		return
	}
	if result.Needs2FA {
		writeData(w, r, 200, result, nil)
		return
	}
	secretJSON, _ := json.Marshal(result.Session)
	conn, err := s.store.UpsertConnection(r.Context(), r.PathValue("account_id"), kind, domain.ConnectionChecking, result.Session.Host, string(secretJSON), "登录成功，等待健康检测")
	if err != nil {
		writeErr(w, r, 500, "connection_save_failed", "无法保存登录态", true, nil)
		return
	}
	job, err := s.createConnectionCheckJob(r.Context(), conn.AccountID, kind)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建检测任务", true, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"needs_2fa": false, "connection": conn, "job": job}, nil)
}

func (s *Server) verifyConnection(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if kind != domain.ConnectionAppleAccount && kind != domain.ConnectionICloudWeb {
		writeErr(w, r, 400, "invalid_connection_kind", "该连接不支持 2FA", false, nil)
		return
	}
	var body struct {
		PendingID   string          `json:"pending_id"`
		Code        string          `json:"code"`
		PhoneNumber json.RawMessage `json:"phone_number,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	session, err := s.protocols.Verify2FA(r.Context(), kind, body.PendingID, body.Code, body.PhoneNumber)
	if err != nil {
		writeErr(w, r, 502, "apple_2fa_failed", err.Error(), true, nil)
		return
	}
	secretJSON, _ := json.Marshal(session)
	conn, err := s.store.UpsertConnection(r.Context(), r.PathValue("account_id"), kind, domain.ConnectionChecking, session.Host, string(secretJSON), "2FA 成功，等待健康检测")
	if err != nil {
		writeErr(w, r, 500, "connection_save_failed", "无法保存登录态", true, nil)
		return
	}
	job, err := s.createConnectionCheckJob(r.Context(), conn.AccountID, kind)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建检测任务", true, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"connection": conn, "job": job}, nil)
}

func (s *Server) createConnectionCheckJob(ctx context.Context, accountID, kind string) (domain.Job, error) {
	job, _, err := s.store.EnsureConnectionCheckJob(ctx, accountID, kind, false)
	if err == nil {
		s.hub.Publish("job.created", job)
	}
	return job, err
}

func (s *Server) checkConnection(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !domain.ValidConnectionKinds[kind] {
		writeErr(w, r, 400, "invalid_connection_kind", "无效连接类型", false, nil)
		return
	}
	job, err := s.createConnectionCheckJob(r.Context(), r.PathValue("account_id"), kind)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建检测任务", true, nil)
		return
	}
	writeData(w, r, 202, job, nil)
}

func (s *Server) setOptionalConnectionEnabled(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	if r.PathValue("kind") != domain.ConnectionAppleAccount {
		writeErr(w, r, 400, "invalid_connection_kind", "此操作仅用于备用创建通道", false, nil)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(r, &body); err != nil || body.Enabled == nil {
		writeErr(w, r, 400, "invalid_request", "请指定是否启用备用创建通道", false, nil)
		return
	}
	conn, err := s.store.SetOptionalConnectionEnabled(r.Context(), r.PathValue("account_id"), *body.Enabled)
	if errors.Is(err, repository.ErrNotFound) {
		writeErr(w, r, 404, "connection_not_found", "请先配置备用创建通道", false, nil)
		return
	}
	if err != nil {
		writeErr(w, r, 500, "connection_update_failed", "无法更新备用创建通道", true, nil)
		return
	}
	s.hub.Publish("connection.changed", map[string]any{"account_id": conn.AccountID, "kind": conn.Kind, "status": conn.Status})
	writeData(w, r, 200, conn, nil)
}
