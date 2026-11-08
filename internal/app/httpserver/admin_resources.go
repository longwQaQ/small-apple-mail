package httpserver

import (
	"net/http"
	"small-apple-mail/internal/app/domain"
	"strconv"
	"strings"
)

func (s *Server) apiClients(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, err := s.store.ListAPIClients(r.Context())
		if err != nil {
			writeErr(w, r, 500, "api_clients_failed", "无法读取 API 客户端", true, nil)
			return
		}
		writeData(w, r, 200, rows, nil)
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	if body.Name == "" || len(body.Scopes) == 0 {
		writeErr(w, r, 400, "invalid_client", "名称和权限域不能为空", false, nil)
		return
	}
	client, key, err := s.store.CreateAPIClient(r.Context(), body.Name, body.Scopes)
	if err != nil {
		writeErr(w, r, 409, "api_client_create_failed", err.Error(), false, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "api_client.created", Result: "success", EntityType: "api_client", EntityID: client.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "具名 API 客户端已创建"})
	writeData(w, r, 201, map[string]any{"client": client, "api_key": key}, nil)
}

func (s *Server) apiClientDetail(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("client_id")
	if r.Method == "DELETE" {
		if err := s.store.DeleteAPIClient(r.Context(), id); err != nil {
			writeErr(w, r, 409, "api_client_delete_failed", err.Error(), false, nil)
			return
		}
		w.WriteHeader(204)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	if err := s.store.SetAPIClientStatus(r.Context(), id, body.Status); err != nil {
		writeErr(w, r, 400, "api_client_update_failed", err.Error(), false, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"id": id, "status": body.Status}, nil)
}

func (s *Server) rotateAPIClient(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	key, err := s.store.RotateAPIClient(r.Context(), r.PathValue("client_id"))
	if err != nil {
		writeErr(w, r, 404, "api_client_not_found", "API 客户端不存在", false, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "api_client.rotated", Result: "success", EntityType: "api_client", EntityID: r.PathValue("client_id"), RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "API Key 已轮换"})
	writeData(w, r, 200, map[string]any{"api_key": key}, nil)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		values, err := s.store.AllSettings(r.Context())
		if err != nil {
			writeErr(w, r, 500, "settings_failed", "无法读取设置", true, nil)
			return
		}
		writeData(w, r, 200, values, nil)
		return
	}
	var body map[string]string
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	allowed := map[string][2]int{"message_retention_days": {1, 365}}
	for key, value := range body {
		bounds, ok := allowed[key]
		if !ok {
			writeErr(w, r, 400, "unknown_setting", "不支持的设置: "+key, false, nil)
			return
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < bounds[0] || n > bounds[1] {
			writeErr(w, r, 400, "invalid_setting", "设置值超出范围: "+key, false, nil)
			return
		}
		if err = s.store.SetSetting(r.Context(), key, strconv.Itoa(n)); err != nil {
			writeErr(w, r, 500, "setting_save_failed", "无法保存设置", true, nil)
			return
		}
	}
	writeData(w, r, 200, map[string]any{"saved": true}, nil)
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, r.Method != "GET"); !ok {
		return
	}
	if r.Method == "POST" {
		var body struct {
			Type   string `json:"type"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		}
		if err := decode(r, &body); err != nil || strings.TrimSpace(body.Type) == "" || (body.Result != "success" && body.Result != "failed") {
			writeErr(w, r, 400, "invalid_activity", "活动类型不能为空，结果必须为 success 或 failed", false, nil)
			return
		}
		e := domain.Activity{Source: "manual", Type: strings.TrimSpace(body.Type), Result: body.Result, Detail: strings.TrimSpace(body.Detail), EntityType: "manual", RequestID: requestID(r), RemoteIP: s.remoteIP(r)}
		if err := s.store.CreateActivity(r.Context(), e); err != nil {
			writeErr(w, r, 500, "activity_create_failed", "无法创建活动", true, nil)
			return
		}
		writeData(w, r, 201, map[string]any{"created": true}, nil)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	rows, total, err := s.store.ListActivitiesPage(r.Context(), page, pageSize)
	if err != nil {
		writeErr(w, r, 500, "activity_failed", "无法读取活动", true, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"items": rows, "page": page, "page_size": pageSize, "total": total}, nil)
}

func (s *Server) activityDetail(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("activity_id"), 10, 64)
	if err != nil || id < 1 {
		writeErr(w, r, 400, "invalid_activity_id", "活动 ID 无效", false, nil)
		return
	}
	if r.Method == "DELETE" {
		if err := s.store.DeleteActivity(r.Context(), id); err != nil {
			writeErr(w, r, 404, "activity_not_found", "活动不存在", false, nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Type   string `json:"type"`
		Result string `json:"result"`
		Detail string `json:"detail"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Type) == "" || (body.Result != "success" && body.Result != "failed") {
		writeErr(w, r, 400, "invalid_activity", "活动类型不能为空，结果必须为 success 或 failed", false, nil)
		return
	}
	if err := s.store.UpdateActivity(r.Context(), id, body.Type, body.Result, body.Detail); err != nil {
		writeErr(w, r, 404, "activity_not_found", "活动不存在", false, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"updated": true}, nil)
}
