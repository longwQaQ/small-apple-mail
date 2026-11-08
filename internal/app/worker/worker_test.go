package worker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/repository"
)

func TestNotificationWakeCancellationAndClear(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "small-apple-mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hub := events.New()
	pool := New(store, hub, slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	started := make(chan struct{}, 1)
	pool.Register("test.block", func(ctx context.Context, _ domain.Job) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if err = pool.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pool.Stop()
	if !pool.Ready() {
		t.Fatal("Worker 未进入就绪状态")
	}

	job, err := store.CreateJob(ctx, "test.block", "{}", 1)
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish("job.created", job)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("通知后 Worker 未立即领取任务")
	}

	deleted, err := pool.ClearJobs(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	jobs, err := store.ListJobs(ctx, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

func TestPartialResultPersistsAndPausesRepeatedScheduleFailures(t *testing.T) {
	ctx := context.Background()
	store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := New(store, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	pool.Register("mailbox.create", func(context.Context, domain.Job) error {
		return &PartialError{Summary: "成功 1 项，待核对 1 项"}
	})
	schedule, err := store.CreateSchedule(ctx, "mailbox.create", "部分完成暂停", "{}", true, 3600)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		job, err := store.CreateJob(ctx, "mailbox.create", "{}", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.DB().ExecContext(ctx, "UPDATE jobs SET schedule_id=? WHERE id=?", schedule.ID, job.ID); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimJob(ctx)
		if err != nil {
			t.Fatal(err)
		}
		pool.run(ctx, claimed)
		got, err := store.Job(ctx, job.ID)
		if err != nil || got.Status != domain.JobPartiallySucceeded {
			t.Fatal(got, err)
		}
		rows, err := store.JobEvents(ctx, job.ID)
		if err != nil || len(rows) != 2 || rows[1].Level != "warning" {
			t.Fatal(rows, err)
		}
	}
	got, err := store.Schedule(ctx, schedule.ID)
	if err != nil || got.ConsecutiveFailures != 3 || got.PausedReason == "" {
		t.Fatal(got, err)
	}
}
