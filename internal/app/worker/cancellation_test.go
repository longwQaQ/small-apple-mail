package worker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/repository"
)

func TestClaimedJobCannotStartAfterCancellation(t *testing.T) {
	for _, action := range []string{"cancel", "clear", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			store, err := repository.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			pool := New(store, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
			ran := false
			pool.Register("test.effect", func(context.Context, domain.Job) error { ran = true; return nil })
			if _, err = store.CreateJob(ctx, "test.effect", "{}", 1); err != nil {
				t.Fatal(err)
			}
			job, err := store.ClaimJob(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if action == "cancel" {
				err = pool.CancelJob(ctx, job.ID)
			} else if action == "clear" {
				_, err = pool.ClearJobs(ctx)
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			pool.run(ctx, job)
			if ran {
				t.Fatal("已取消或已清空的任务仍然执行了处理器")
			}
			if action != "clear" {
				current, err := store.Job(context.Background(), job.ID)
				if err != nil || current.Status != domain.JobCancelled {
					t.Fatalf("status=%s err=%v", current.Status, err)
				}
			}
		})
	}
}
