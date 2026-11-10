package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"strconv"
	"strings"
	"time"
)

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		rows, next, err := s.store.ListJobsPage(r.Context(), r.URL.Query().Get("cursor"), cutoff, limit)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				writeErr(w, r, 400, "invalid_cursor", "任务分页游标无效或已过期", false, nil)
				return
			}
			writeErr(w, r, 500, "jobs_failed", "无法读取任务", true, nil)
			return
		}
		writeData(w, r, 200, map[string]any{"items": rows, "next_cursor": next, "retention_hours": 24}, nil)
		return
	}
	if r.Method == "DELETE" {
		var deleted int64
		var err error
		if s.jobController != nil {
			deleted, err = s.jobController.ClearJobs(r.Context())
		} else {
			deleted, err = s.store.DeleteAllJobs(r.Context())
		}
		if err != nil {
			writeErr(w, r, 500, "jobs_delete_failed", "无法删除任务记录", true, nil)
			return
		}
		writeData(w, r, 200, map[string]any{"deleted": deleted}, nil)
		return
	}
	var body struct {
		Type     string `json:"type"`
		Payload  any    `json:"payload"`
		Priority int    `json:"priority"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	body.Type = strings.TrimSpace(body.Type)
	if !validScheduleTypes[body.Type] {
		writeErr(w, r, 400, "invalid_job_type", "不支持的任务类型", false, nil)
		return
	}
	payload, _ := json.Marshal(body.Payload)
	job, err := s.store.CreateJob(r.Context(), body.Type, string(payload), body.Priority)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建任务", true, nil)
		return
	}
	s.hub.Publish("job.created", job)
	writeData(w, r, 202, job, nil)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	var err error
	if s.jobController != nil {
		err = s.jobController.CancelJob(r.Context(), r.PathValue("job_id"))
	} else {
		err = s.store.CancelJob(r.Context(), r.PathValue("job_id"))
	}
	if err != nil {
		writeErr(w, r, 404, "job_not_found", "任务不存在或不可取消", false, nil)
		return
	}
	s.hub.Publish("job.progress", map[string]any{"id": r.PathValue("job_id"), "status": "cancelling"})
	writeData(w, r, 200, map[string]any{"cancelled": true}, nil)
}

func (s *Server) jobDetail(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	job, err := s.store.Job(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeErr(w, r, 404, "job_not_found", "任务不存在", false, nil)
		return
	}
	writeData(w, r, 200, job, nil)
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	rows, err := s.store.JobEvents(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeErr(w, r, 500, "job_events_failed", "无法读取任务事件", true, nil)
		return
	}
	writeData(w, r, 200, rows, nil)
}

func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if r.Method == "GET" {
		rows, err := s.store.ListSchedules(r.Context())
		if err != nil {
			writeErr(w, r, 500, "schedules_failed", "无法读取计划", true, nil)
			return
		}
		writeData(w, r, 200, rows, nil)
		return
	}
	var body struct {
		Type     string          `json:"type"`
		Name     string          `json:"name"`
		Payload  json.RawMessage `json:"payload"`
		Enabled  *bool           `json:"enabled"`
		Interval int             `json:"interval_seconds"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	body.Type = strings.TrimSpace(body.Type)
	if !validScheduleTypes[body.Type] {
		writeErr(w, r, 400, "invalid_schedule_type", "不支持的计划任务类型", false, nil)
		return
	}
	payload := strings.TrimSpace(string(body.Payload))
	if payload == "" || payload == "null" {
		payload = "{}"
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	schedule, err := s.store.CreateSchedule(r.Context(), body.Type, body.Name, payload, enabled, body.Interval)
	if err != nil {
		writeErr(w, r, 400, "schedule_create_failed", err.Error(), false, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "schedule.created", Result: "success", EntityType: "schedule", EntityID: schedule.ID, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "定时计划已创建"})
	s.hub.Publish("system.notice", map[string]any{"kind": "schedule.created", "schedule_id": schedule.ID})
	writeData(w, r, 201, schedule, nil)
}

func (s *Server) scheduleDetail(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, r.Method != "GET")
	if !ok {
		return
	}
	id := r.PathValue("schedule_id")
	if r.Method == "GET" {
		schedule, err := s.store.Schedule(r.Context(), id)
		if err != nil {
			writeErr(w, r, 404, "schedule_not_found", "计划不存在", false, nil)
			return
		}
		writeData(w, r, 200, schedule, nil)
		return
	}
	if r.Method == "DELETE" {
		if err := s.store.DeleteSchedule(r.Context(), id); err != nil {
			writeErr(w, r, 404, "schedule_not_found", "计划不存在", false, nil)
			return
		}
		_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "schedule.deleted", Result: "success", EntityType: "schedule", EntityID: id, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "定时计划已删除"})
		s.hub.Publish("system.notice", map[string]any{"kind": "schedule.deleted", "schedule_id": id})
		w.WriteHeader(204)
		return
	}
	current, err := s.store.Schedule(r.Context(), id)
	if err != nil {
		writeErr(w, r, 404, "schedule_not_found", "计划不存在", false, nil)
		return
	}
	var body struct {
		Type     *string         `json:"type"`
		Name     *string         `json:"name"`
		Payload  json.RawMessage `json:"payload"`
		Enabled  *bool           `json:"enabled"`
		Interval *int            `json:"interval_seconds"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, r, 400, "invalid_request", err.Error(), false, nil)
		return
	}
	typ, name, payload, enabled, interval := current.Type, current.Name, current.Payload, current.Enabled, current.IntervalSeconds
	if body.Type != nil {
		typ = strings.TrimSpace(*body.Type)
	}
	if !validScheduleTypes[typ] {
		writeErr(w, r, 400, "invalid_schedule_type", "不支持的计划任务类型", false, nil)
		return
	}
	if body.Name != nil {
		name = *body.Name
	}
	if len(body.Payload) > 0 {
		payload = strings.TrimSpace(string(body.Payload))
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	if body.Interval != nil {
		interval = *body.Interval
	}
	schedule, err := s.store.UpdateSchedule(r.Context(), id, typ, name, payload, enabled, interval)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeErr(w, r, 404, "schedule_not_found", "计划不存在", false, nil)
			return
		}
		writeErr(w, r, 400, "schedule_update_failed", err.Error(), false, nil)
		return
	}
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "schedule.updated", Result: "success", EntityType: "schedule", EntityID: id, RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "定时计划已更新"})
	s.hub.Publish("system.notice", map[string]any{"kind": "schedule.updated", "schedule_id": id})
	writeData(w, r, 200, schedule, nil)
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	job, err := s.store.RunScheduleNow(r.Context(), r.PathValue("schedule_id"))
	if err != nil {
		writeErr(w, r, 404, "schedule_not_found", "计划不存在", false, nil)
		return
	}
	s.hub.Publish("job.created", job)
	_ = s.store.CreateActivity(r.Context(), domain.Activity{Type: "schedule.run", Result: "success", EntityType: "schedule", EntityID: r.PathValue("schedule_id"), RequestID: requestID(r), RemoteIP: s.remoteIP(r), Detail: "定时计划已手动运行"})
	writeData(w, r, 202, job, nil)
}
