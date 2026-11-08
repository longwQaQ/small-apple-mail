package httpserver

import (
	"net/http"
	"strconv"
	"strings"

	"small-apple-mail/internal/app/repository"
)

func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAdmin(w, r, false); !ok {
		return
	}
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	size, _ := strconv.Atoi(query.Get("page_size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 50 {
		size = 20
	}
	kind := query.Get("kind")
	if kind != "" && kind != "code" && kind != "link" && kind != "ordinary" {
		writeErr(w, r, 400, "invalid_kind", "请选择全部邮件、验证码、登录链接或普通邮件", false, nil)
		return
	}
	rows, total, err := s.store.ListInbox(r.Context(), repository.InboxFilter{Query: strings.TrimSpace(query.Get("query")), AccountID: query.Get("account_id"), Kind: kind, Page: page, PageSize: size})
	if err != nil {
		writeErr(w, r, 500, "inbox_failed", "无法读取收件箱", true, nil)
		return
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	writeData(w, r, 200, map[string]any{"items": rows, "total": total, "page": page, "page_size": size, "pages": pages}, nil)
}
