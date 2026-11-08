package providers

import (
	"context"
	"fmt"

	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/worker"
)

type batchResult struct {
	ctx                                        context.Context
	store                                      *repository.Store
	hub                                        *events.Hub
	jobID                                      string
	total, success, failed, uncertain, skipped int
}

func (b *batchResult) record(targetID, target, status, message string) error {
	// 取消只能停止后续操作，已经发出的请求仍需留下结果供核对。
	if err := b.store.AddJobItem(context.WithoutCancel(b.ctx), b.jobID, targetID, target, status, message, b.total); err != nil {
		return err
	}
	switch status {
	case "succeeded":
		b.success++
	case "failed":
		b.failed++
	case "uncertain":
		b.uncertain++
	case "skipped":
		b.skipped++
	}
	b.hub.Publish("job.progress", map[string]any{"id": b.jobID})
	return nil
}

func (b *batchResult) finish() error {
	if b.failed == 0 && b.uncertain == 0 && b.skipped == 0 && b.success > 0 {
		return nil
	}
	summary := fmt.Sprintf("成功 %d 项，失败 %d 项，待核对 %d 项，跳过 %d 项", b.success, b.failed, b.uncertain, b.skipped)
	if b.success > 0 {
		return &worker.PartialError{Summary: summary}
	}
	return fmt.Errorf("%s；请查看任务明细", summary)
}
