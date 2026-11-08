package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/worker"
)

type mailboxCreateError struct{ err error }

func (e *mailboxCreateError) Error() string { return e.err.Error() }
func (e *mailboxCreateError) Unwrap() error { return e.err }

func selectedAccounts(ctx context.Context, store *repository.Store, ids []string) ([]domain.Account, error) {
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	result := []domain.Account{}
	for _, account := range accounts {
		if len(wanted) == 0 || wanted[account.ID] {
			result = append(result, account)
		}
	}
	if len(wanted) > 0 && len(result) != len(wanted) {
		return nil, fmt.Errorf("部分账号已不存在，请重新选择")
	}
	return result, nil
}

func (s *ConnectionService) createMailboxesJob() worker.Handler {
	return func(ctx context.Context, job domain.Job) error {
		var payload struct {
			AccountIDs     []string `json:"account_ids"`
			Count          int      `json:"count"`
			Label          string   `json:"label"`
			Note           string   `json:"note"`
			Strategy       string   `json:"strategy"`
			AllocationMode string   `json:"allocation_mode"`
			Purpose        string   `json:"purpose"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return err
		}
		if payload.Count == 0 {
			payload.Count = 1
		}
		if payload.Count < 1 || payload.Count > 100 {
			return fmt.Errorf("每个账号创建数量必须为 1–100")
		}
		if payload.AllocationMode == "" {
			payload.AllocationMode = "shared"
		}
		if payload.AllocationMode != "shared" && payload.AllocationMode != "dedicated" {
			return fmt.Errorf("无效邮箱用途类型")
		}
		if len([]rune(payload.Purpose)) > 120 {
			return fmt.Errorf("用途说明最多 120 个字符")
		}
		accounts, err := selectedAccounts(ctx, s.store, payload.AccountIDs)
		if err != nil {
			return err
		}
		batch := batchResult{ctx: ctx, store: s.store, hub: s.hub, jobID: job.ID, total: len(accounts) * payload.Count}
		for _, account := range accounts {
			stopped := false
			for i := 0; i < payload.Count; i++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				target := fmt.Sprintf("%s · 第 %d 个", account.AppleID, i+1)
				if stopped {
					if err = batch.record(account.ID, target, "skipped", "该账号前序创建未完成，请处理后再执行"); err != nil {
						return err
					}
					continue
				}
				remote, createErr := s.createRemoteMailbox(ctx, account.ID, payload.Strategy, payload.Label, payload.Note)
				status, message := "succeeded", remote.Email
				if createErr != nil {
					status, message = "failed", createErr.Error()
					var uncertain *mailboxCreateError
					if errors.As(createErr, &uncertain) {
						status = "uncertain"
						message += "；请求已发出，请同步远端核对后再创建"
					}
					stopped = true
				} else {
					mailbox, saveErr := s.store.SaveCreatedRemoteMailbox(ctx, account.ID, repository.RemoteMailboxInput{AnonymousID: remote.AnonymousID, Email: remote.Email, Label: remote.Label, Note: remote.Note, Active: remote.IsActive, CreatedAt: remote.CreatedAt, AllocationMode: payload.AllocationMode, Purpose: payload.Purpose})
					if saveErr != nil {
						status, message = "uncertain", "远端已创建 "+remote.Email+"，本地保存未完成："+saveErr.Error()
						stopped = true
					} else {
						s.hub.Publish("mailbox.changed", mailbox)
					}
				}
				if err = batch.record(account.ID, target, status, message); err != nil {
					return err
				}
			}
		}
		return batch.finish()
	}
}

func (s *ConnectionService) remoteSyncJob() worker.Handler {
	return func(ctx context.Context, job domain.Job) error {
		var payload struct {
			AccountIDs []string `json:"account_ids"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return err
		}
		accounts, err := selectedAccounts(ctx, s.store, payload.AccountIDs)
		if err != nil {
			return err
		}
		batch := batchResult{ctx: ctx, store: s.store, hub: s.hub, jobID: job.ID, total: len(accounts)}
		for _, account := range accounts {
			if err := ctx.Err(); err != nil {
				return err
			}
			remote, err := s.ListPrivacyMailboxes(ctx, account.ID)
			status, message := "succeeded", fmt.Sprintf("已同步 %d 个邮箱，本地用途和标签保留", len(remote))
			if err == nil {
				items := make([]repository.RemoteMailboxInput, 0, len(remote))
				for _, m := range remote {
					items = append(items, repository.RemoteMailboxInput{AnonymousID: m.AnonymousID, Email: m.Email, Label: m.Label, Note: m.Note, Active: m.IsActive, CreatedAt: m.CreatedAt})
				}
				_, err = s.store.SyncRemoteMailboxes(ctx, account.ID, items)
			}
			if err != nil {
				status, message = "failed", err.Error()
				if errors.Is(err, repository.ErrNotFound) {
					status, message = "skipped", "尚未配置 iCloud 邮箱管理连接"
				}
			} else {
				s.hub.Publish("mailbox.changed", map[string]any{"account_id": account.ID})
			}
			if err = batch.record(account.ID, account.AppleID, status, message); err != nil {
				return err
			}
		}
		return batch.finish()
	}
}
