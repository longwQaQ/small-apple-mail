package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"small-apple-mail/internal/app/repository"
)

func (s *Server) mailboxOverview(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	var total, available, dedicated, leased, attention int
	err := s.store.DB().QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(status='available' AND allocation_mode='shared'),0),COALESCE(SUM(allocation_mode='dedicated'),0),COALESCE(SUM(status='leased'),0),COALESCE(SUM(status='remote_missing' OR remote_status='unknown'),0) FROM mailboxes`).Scan(&total, &available, &dedicated, &leased, &attention)
	if err != nil {
		writeErr(w, r, 500, "overview_failed", "无法读取邮箱概览", true, nil)
		return
	}
	writeData(w, r, 200, map[string]any{"total": total, "available": available, "dedicated": dedicated, "leased": leased, "attention": attention}, nil)
}

func (s *Server) batchMailboxes(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, true); !ok {
		return
	}
	var body struct {
		IDs   []string                `json:"ids"`
		Patch repository.MailboxPatch `json:"patch"`
	}
	if err := decode(r, &body); err != nil || len(body.IDs) == 0 || len(body.IDs) > 200 {
		writeErr(w, r, 400, "invalid_batch", "请选择 1–200 个邮箱", false, nil)
		return
	}
	type item struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	result := []item{}
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, err := s.store.PatchMailbox(r.Context(), id, body.Patch)
		row := item{ID: id, Success: err == nil}
		if err != nil {
			row.Error = err.Error()
		}
		result = append(result, row)
	}
	s.hub.Publish("mailbox.changed", map[string]any{"batch": true})
	writeData(w, r, 200, map[string]any{"items": result}, nil)
}

func (s *Server) mailboxResult(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	mailbox, err := s.store.Mailbox(r.Context(), r.PathValue("mailbox_id"))
	if err != nil {
		writeErr(w, r, 404, "mailbox_not_found", "邮箱不存在", false, nil)
		return
	}
	query := r.URL.Query()
	kind := query.Get("kind")
	if kind != "" && kind != "all" && kind != "code" && kind != "link" {
		writeErr(w, r, 400, "invalid_kind", "请选择验证码、登录链接或全部结果", false, nil)
		return
	}
	after, err := time.Parse(time.RFC3339Nano, query.Get("after"))
	if err != nil {
		writeErr(w, r, 400, "invalid_after", "请提供有效的等待起始时间", false, nil)
		return
	}
	wait, err := strconv.Atoi(query.Get("wait_seconds"))
	if err != nil || wait < 0 || wait > 30 {
		writeErr(w, r, 400, "invalid_wait", "等待时间必须为 0–30 秒", false, nil)
		return
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	triggered := false
	for {
		result, err := s.store.FindMailboxResult(r.Context(), mailbox.ID, kind, strings.TrimSpace(query.Get("keyword")), after)
		if err == nil {
			writeData(w, r, 200, map[string]any{"message": result, "waiting": false}, nil)
			return
		}
		if !errors.Is(err, repository.ErrNotFound) {
			writeErr(w, r, 500, "result_failed", "查询收件结果失败", true, nil)
			return
		}
		if !triggered {
			job, created, err := s.store.EnsureMessageSyncJob(r.Context(), mailbox.AccountID, 15)
			if err != nil {
				writeErr(w, r, 500, "sync_failed", "无法安排收件同步", true, nil)
				return
			}
			if created {
				s.hub.Publish("job.created", job)
			}
			triggered = true
		}
		if !time.Now().Before(deadline) {
			writeData(w, r, 200, map[string]any{"message": nil, "waiting": true}, nil)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
