package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/worker"
)

func TestCreateBatchStopsAfterUncertainRequestAndPreservesResults(t *testing.T) {
	s, c := connectionFixture(t)
	ctx := context.Background()
	session := protocol.ICloudSession{Host: "www.icloud.com", DSID: "fixture", PremiumMailBaseURL: "https://test.icloud.com", IsICloudPlus: true, CanCreateHME: true, Cookies: []protocol.SessionCookie{{Name: "test", Value: "fixture", Domain: ".icloud.com", Path: "/"}}}
	encoded, _ := json.Marshal(session)
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, c.Kind, domain.ConnectionHealthy, session.Host, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls, reserves := 0, 0
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"success":true,"result":{"hme":"alias@example.invalid"}}`
		if strings.HasSuffix(r.URL.Path, "/reserve") {
			reserves++
			if reserves == 2 {
				return nil, io.ErrUnexpectedEOF
			}
			body = `{"success":true,"result":{"hme":{"hme":"alias@example.invalid","anonymousId":"fixture","isActive":true}}}`
		}
		return &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	payload, _ := json.Marshal(map[string]any{"account_ids": []string{c.AccountID}, "count": 3, "allocation_mode": "dedicated", "purpose": "固定服务"})
	job, err := s.store.CreateJob(ctx, "mailbox.create", string(payload), 1)
	if err != nil {
		t.Fatal(err)
	}
	err = s.createMailboxesJob()(ctx, job)
	var partial *worker.PartialError
	if !errors.As(err, &partial) || calls != 4 {
		t.Fatalf("不应重放不确定的创建请求或继续第三项: calls=%d err=%v", calls, err)
	}
	items, err := s.store.JobItems(ctx, job.ID)
	if err != nil || len(items) != 3 {
		t.Fatal(items, err)
	}
	for i, status := range []string{"succeeded", "uncertain", "skipped"} {
		if items[i].Status != status {
			t.Fatal(items)
		}
	}
	mailboxes, err := s.store.AllMailboxesByAccount(ctx, c.AccountID)
	if err != nil || len(mailboxes) != 1 || mailboxes[0].AllocationMode != "dedicated" || mailboxes[0].Purpose != "固定服务" {
		t.Fatal(mailboxes, err)
	}
}

func TestCancelledCreateKeepsUncertainResult(t *testing.T) {
	s, c := connectionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := protocol.ICloudSession{Host: "www.icloud.com", DSID: "fixture", PremiumMailBaseURL: "https://test.icloud.com", IsICloudPlus: true, CanCreateHME: true, Cookies: []protocol.SessionCookie{{Name: "test", Value: "fixture", Domain: ".icloud.com", Path: "/"}}}
	encoded, _ := json.Marshal(session)
	if _, err := s.store.UpsertConnection(ctx, c.AccountID, c.Kind, domain.ConnectionHealthy, session.Host, string(encoded), ""); err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = maintenanceTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/reserve") {
			cancel()
			return nil, context.Canceled
		}
		return &http.Response{StatusCode: 200, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{"hme":"alias@example.invalid"}}`))}, nil
	})
	payload, _ := json.Marshal(map[string]any{"account_ids": []string{c.AccountID}, "count": 2})
	job, err := s.store.CreateJob(ctx, "mailbox.create", string(payload), 1)
	if err != nil {
		t.Fatal(err)
	}
	err = s.createMailboxesJob()(ctx, job)
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatal("取消后不应继续创建", calls, err)
	}
	items, err := s.store.JobItems(context.Background(), job.ID)
	if err != nil || len(items) != 1 || items[0].Status != "uncertain" {
		t.Fatal("请求发出后取消，仍需保留待核对结果", items, err)
	}
}
