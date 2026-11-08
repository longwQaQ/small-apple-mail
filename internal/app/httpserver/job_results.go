package httpserver

import (
	"encoding/json"
	"net/http"

	"small-apple-mail/internal/app/domain"
)

func (s *Server) jobItems(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	if _, err := s.store.Job(r.Context(), r.PathValue("job_id")); err != nil {
		writeErr(w, r, 404, "job_not_found", "任务不存在", false, nil)
		return
	}
	rows, err := s.store.JobItems(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeErr(w, r, 500, "job_items_failed", "读取任务明细失败", true, nil)
		return
	}
	writeData(w, r, 200, rows, nil)
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	job, err := s.store.Job(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeErr(w, r, 404, "job_not_found", "任务不存在", false, nil)
		return
	}
	if job.Status != domain.JobFailed && job.Status != domain.JobPartiallySucceeded {
		writeErr(w, r, 409, "job_not_retryable", "任务尚未失败，不能重复执行", false, nil)
		return
	}
	if job.Type != "mailbox.remote_sync" && job.Type != "mailbox.message_sync" {
		writeErr(w, r, 400, "job_not_retryable", "此任务不支持自动重试；创建结果待核对时，请先同步远端邮箱", false, nil)
		return
	}
	items, err := s.store.JobItems(r.Context(), job.ID)
	if err != nil {
		writeErr(w, r, 500, "job_items_failed", "读取失败项失败", true, nil)
		return
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		if item.Status == "failed" && !seen[item.TargetID] {
			ids = append(ids, item.TargetID)
			seen[item.TargetID] = true
		}
	}
	if len(ids) == 0 {
		writeErr(w, r, 400, "nothing_to_retry", "没有可重试的失败账号", false, nil)
		return
	}
	var payload map[string]any
	if json.Unmarshal([]byte(job.Payload), &payload) != nil || payload == nil {
		payload = map[string]any{}
	}
	delete(payload, "account_id")
	payload["account_ids"] = ids
	encoded, _ := json.Marshal(payload)
	next, err := s.store.CreateJob(r.Context(), job.Type, string(encoded), job.Priority)
	if err != nil {
		writeErr(w, r, 500, "job_create_failed", "无法创建重试任务", true, nil)
		return
	}
	s.hub.Publish("job.created", next)
	writeData(w, r, 202, next, nil)
}
