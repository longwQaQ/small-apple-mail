package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/repository"
)

type Handler func(context.Context, domain.Job) error

type PartialError struct{ Summary string }

func (e *PartialError) Error() string { return e.Summary }

type Pool struct {
	store       *repository.Store
	hub         *events.Hub
	logger      *slog.Logger
	concurrency int
	handlers    map[string]Handler
	wake        chan struct{}
	cancel      context.CancelFunc
	unsubscribe func()
	wg          sync.WaitGroup
	runMu       sync.Mutex
	clearMu     sync.Mutex
	running     map[string]context.CancelFunc
	ready       atomic.Bool
	clearing    atomic.Bool
}

func New(store *repository.Store, hub *events.Hub, logger *slog.Logger, concurrency int) *Pool {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 16 {
		concurrency = 16
	}
	return &Pool{store: store, hub: hub, logger: logger, concurrency: concurrency, handlers: map[string]Handler{
		"database.integrity_check": func(ctx context.Context, _ domain.Job) error { return store.IntegrityCheck(ctx) },
	}, wake: make(chan struct{}, concurrency), running: map[string]context.CancelFunc{}}
}

func (p *Pool) Register(kind string, handler Handler) { p.handlers[kind] = handler }

func (p *Pool) Start(parent context.Context) error {
	if err := p.store.InterruptRunningJobs(parent); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	eventsCh, unsubscribe := p.hub.Subscribe(0)
	p.unsubscribe = unsubscribe
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-eventsCh:
				if !ok {
					return
				}
				if event.Type == "job.created" {
					p.Notify()
				}
				if event.Type == "job.progress" {
					var update struct{ ID, Status string }
					if json.Unmarshal(event.Data, &update) == nil && update.Status == domain.JobCancelling {
						p.cancelRunning(update.ID)
					}
				}
			}
		}
	}()
	for i := 0; i < p.concurrency; i++ {
		p.wg.Add(1)
		go p.loop(ctx)
	}
	p.ready.Store(true)
	p.Notify()
	return nil
}

func (p *Pool) Ready() bool { return p.ready.Load() }

func (p *Pool) Notify() {
	for i := 0; i < p.concurrency; i++ {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) Stop() {
	p.ready.Store(false)
	if p.cancel != nil {
		p.cancel()
	}
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
	p.runMu.Lock()
	for _, cancel := range p.running {
		cancel()
	}
	p.runMu.Unlock()
	p.wg.Wait()
}

func (p *Pool) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
		if p.clearing.Load() {
			continue
		}
		for !p.clearing.Load() {
			job, err := p.store.ClaimJob(ctx)
			if errors.Is(err, repository.ErrNotFound) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					p.logger.Error("claim job", "err", err)
				}
				break
			}
			p.run(ctx, job)
		}
	}
}

func (p *Pool) run(parent context.Context, job domain.Job) {
	ctx, cancel := context.WithCancel(parent)
	p.runMu.Lock()
	current, err := p.store.Job(context.WithoutCancel(parent), job.ID)
	if err != nil || current.Status != domain.JobRunning || p.clearing.Load() || ctx.Err() != nil {
		if err == nil && (current.Status == domain.JobRunning || current.Status == domain.JobCancelling) {
			_ = p.store.CompleteJob(context.Background(), job.ID, domain.JobCancelled, "job_cancelled", "任务已取消")
			p.hub.Publish("job.completed", map[string]any{"id": job.ID, "status": domain.JobCancelled})
		} else if err != nil && !errors.Is(err, repository.ErrNotFound) && parent.Err() == nil {
			p.logger.Error("load claimed job", "id", job.ID, "err", err)
		}
		p.runMu.Unlock()
		cancel()
		return
	}
	p.running[job.ID] = cancel
	p.runMu.Unlock()
	defer func() {
		cancel()
		p.runMu.Lock()
		delete(p.running, job.ID)
		p.runMu.Unlock()
	}()
	p.hub.Publish("job.progress", job)
	_, _ = p.store.AddJobEvent(ctx, job.ID, "start", "info", "任务开始", "{}")
	handler := p.handlers[job.Type]
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if handler == nil {
		err = errors.New("该任务类型尚未注册执行器")
	} else {
		err = handler(ctx, job)
	}
	if ctx.Err() != nil {
		_ = p.store.CompleteJob(context.Background(), job.ID, domain.JobCancelled, "job_cancelled", "任务已取消")
		_, _ = p.store.AddJobEvent(context.Background(), job.ID, "finish", "warning", "任务已取消", "{}")
		p.hub.Publish("job.completed", map[string]any{"id": job.ID, "status": domain.JobCancelled})
		return
	}
	if err != nil {
		status := domain.JobFailed
		code, level, message := "job_failed", "error", "任务失败"
		var partial *PartialError
		if errors.As(err, &partial) {
			status = domain.JobPartiallySucceeded
			code, level, message = "job_partially_succeeded", "warning", "任务部分完成，请查看逐项结果"
		}
		_ = p.store.CompleteJob(ctx, job.ID, status, code, err.Error())
		_, _ = p.store.AddJobEvent(ctx, job.ID, "finish", level, message, "{}")
		p.hub.Publish("job.completed", map[string]any{"id": job.ID, "status": status, "error": err.Error()})
		return
	}
	_ = p.store.CompleteJob(ctx, job.ID, domain.JobSucceeded, "", "")
	_, _ = p.store.AddJobEvent(ctx, job.ID, "finish", "info", "任务完成", "{}")
	p.hub.Publish("job.completed", map[string]any{"id": job.ID, "status": domain.JobSucceeded})
}

func (p *Pool) cancelRunning(id string) {
	p.runMu.Lock()
	cancel := p.running[id]
	p.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Pool) CancelJob(ctx context.Context, id string) error {
	if err := p.store.CancelJob(ctx, id); err != nil {
		return err
	}
	p.cancelRunning(id)
	return nil
}

func (p *Pool) ClearJobs(ctx context.Context) (int64, error) {
	p.clearMu.Lock()
	defer p.clearMu.Unlock()
	p.clearing.Store(true)
	defer func() {
		p.clearing.Store(false)
		p.Notify()
	}()
	if err := p.store.CancelAllJobs(ctx); err != nil {
		return 0, err
	}
	p.runMu.Lock()
	for _, cancel := range p.running {
		cancel()
	}
	p.runMu.Unlock()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.runMu.Lock()
		count := len(p.running)
		p.runMu.Unlock()
		if count == 0 {
			return p.store.DeleteAllJobs(ctx)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}
