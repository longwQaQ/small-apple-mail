package httpserver

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/security"
	"small-apple-mail/internal/app/web"
	"strconv"
	"strings"
	"time"
)

func (s *Server) pageApp(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := web.FS.ReadFile("templates/app.html")
	if err != nil {
		http.Error(w, "page unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	data, err := web.FS.ReadFile("templates/login.html")
	if err != nil {
		http.Error(w, "page unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if name != r.PathValue("name") {
		http.NotFound(w, r)
		return
	}
	data, err := web.FS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	switch filepath.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	_, _ = w.Write(data)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeData(w, r, http.StatusOK, map[string]any{"status": "ok"}, nil)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ready(r.Context()); err != nil {
		writeErr(w, r, http.StatusServiceUnavailable, "not_ready", "数据库不可用", true, nil)
		return
	}
	if s.jobController != nil && !s.jobController.Ready() {
		writeErr(w, r, http.StatusServiceUnavailable, "worker_not_ready", "任务执行器尚未就绪", true, nil)
		return
	}
	version, _ := s.store.SchemaVersion(r.Context())
	writeData(w, r, http.StatusOK, map[string]any{"status": "ready", "schema_version": version}, nil)
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求 JSON 无效: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("请求只能包含一个 JSON 对象")
	}
	return nil
}

func writeData(w http.ResponseWriter, r *http.Request, status int, data any, meta map[string]any) {
	if meta == nil {
		meta = map[string]any{}
	}
	meta["request_id"] = requestID(r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": meta})
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, message string, retry bool, details any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "retryable": retry, "request_id": requestID(r), "details": details}})
}

func (s *Server) remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !s.isTrustedProxy(peer) {
		return host
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(forwarded) == 0 || len(forwarded) > 32 {
		return host
	}
	candidate := peer
	for i := len(forwarded) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if ip == nil {
			return host
		}
		candidate = ip
		if !s.isTrustedProxy(ip) {
			return ip.String()
		}
	}
	return candidate.String()
}

func (s *Server) isTrustedProxy(ip net.IP) bool {
	for _, network := range s.trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) loginBlocked(key string) (bool, time.Duration) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	now := time.Now()
	cut := now.Add(-15 * time.Minute)
	values := s.failures[key][:0]
	for _, v := range s.failures[key] {
		if v.After(cut) {
			values = append(values, v)
		}
	}
	s.failures[key] = values
	if len(values) >= 8 {
		return true, time.Until(values[0].Add(15 * time.Minute))
	}
	return false, 0
}

func (s *Server) recordFailure(key string) {
	s.failMu.Lock()
	s.failures[key] = append(s.failures[key], time.Now())
	s.failMu.Unlock()
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	key := s.remoteIP(r) + "|" + strings.ToLower(strings.TrimSpace(body.Username))
	if blocked, wait := s.loginBlocked(key); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, r, 429, "login_rate_limited", "登录尝试过多，请稍后再试", true, map[string]any{"retry_after_seconds": int(wait.Seconds()) + 1})
		return
	}
	admin, err := s.store.AdminByUsername(r.Context(), body.Username)
	if err != nil || !security.VerifyPassword(body.Password, admin.PasswordHash) {
		s.recordFailure(key)
		time.Sleep(150 * time.Millisecond)
		writeErr(w, r, 401, "invalid_credentials", "管理员账号或密码错误", false, nil)
		return
	}
	session, token, csrf, err := s.store.CreateSession(r.Context(), s.cfg.IdleDuration(), s.cfg.AbsoluteDuration())
	if err != nil {
		writeErr(w, r, 500, "session_create_failed", "无法创建登录会话", true, nil)
		return
	}
	_ = s.store.MarkAdminLogin(r.Context(), admin.ID)
	s.setSessionCookie(w, token, session.AbsoluteExpiresAt)
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "admin.login", Result: "success", EntityType: "admin", EntityID: admin.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "管理员登录成功"})
	writeData(w, r, 200, map[string]any{"admin": admin, "csrf_token": csrf}, nil)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, Expires: time.Unix(1, 0), MaxAge: -1})
}

func (s *Server) adminSession(r *http.Request, csrf bool) (domain.Admin, domain.Session, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return domain.Admin{}, domain.Session{}, repository.ErrNotFound
	}
	session, err := s.store.SessionByToken(r.Context(), cookie.Value, s.cfg.IdleDuration())
	if err != nil {
		return domain.Admin{}, domain.Session{}, err
	}
	if csrf && !security.EqualHash(r.Header.Get("X-CSRF-Token"), session.CSRFHash) {
		return domain.Admin{}, session, errors.New("csrf_invalid")
	}
	admin, err := s.store.Admin(r.Context())
	return admin, session, err
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, csrf bool) (domain.Admin, domain.Session, bool) {
	a, sess, err := s.adminSession(r, csrf)
	if err != nil {
		code := "auth_required"
		message := "请先登录"
		status := 401
		if err.Error() == "csrf_invalid" {
			code = "csrf_invalid"
			message = "安全令牌已失效，请刷新页面"
			status = 403
		}
		writeErr(w, r, status, code, message, false, nil)
		return a, sess, false
	}
	return a, sess, true
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	csrf := security.CSRFToken(sess.TokenHash)
	writeData(w, r, 200, map[string]any{"admin": admin, "csrf_token": csrf, "expires_at": sess.AbsoluteExpiresAt}, nil)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	_ = s.store.DeleteSession(r.Context(), sess.ID)
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	admin, sess, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	if !security.VerifyPassword(body.Current, admin.PasswordHash) {
		writeErr(w, r, 400, "invalid_current_password", "当前密码错误", false, nil)
		return
	}
	hash, err := security.HashPassword(body.New)
	if err != nil {
		writeErr(w, r, 400, "weak_password", err.Error(), false, nil)
		return
	}
	if err = s.store.ChangePassword(r.Context(), admin.ID, hash, sess.ID); err != nil {
		writeErr(w, r, 500, "password_update_failed", "无法修改密码", true, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "admin.password_changed", Result: "success", EntityType: "admin", EntityID: admin.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "管理员密码已修改，其他会话已撤销"})
	writeData(w, r, 200, map[string]any{"changed": true}, nil)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	data, err := s.store.Dashboard(r.Context())
	if err != nil {
		writeErr(w, r, 500, "dashboard_failed", "无法读取仪表盘", true, nil)
		return
	}
	writeData(w, r, 200, data, nil)
}
