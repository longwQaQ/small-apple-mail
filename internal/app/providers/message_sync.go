package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/services"
	"small-apple-mail/internal/app/worker"
)

type messagePageFetcher func(context.Context, protocol.LoginState, []protocol.Mailbox, time.Time, protocol.IMAPCursor) (protocol.IMAPSyncResult, error)

func messageSyncHandler(store *repository.Store, hub *events.Hub, fetch messagePageFetcher) worker.Handler {
	return func(ctx context.Context, job domain.Job) error {
		var payload struct {
			AccountID    string   `json:"account_id"`
			AccountIDs   []string `json:"account_ids"`
			LookbackDays int      `json:"lookback_days"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return err
		}
		if payload.LookbackDays < 0 || payload.LookbackDays > 365 {
			return fmt.Errorf("补同步范围必须为 1–365 天")
		}
		ids := append([]string(nil), payload.AccountIDs...)
		if payload.AccountID != "" {
			ids = append(ids, payload.AccountID)
		}
		accounts, err := selectedAccounts(ctx, store, ids)
		if err != nil {
			return err
		}
		batch := batchResult{ctx: ctx, store: store, hub: hub, jobID: job.ID, total: len(accounts)}
		for _, account := range accounts {
			if err := ctx.Err(); err != nil {
				return err
			}
			saved, err := syncAccountMessages(ctx, store, account.ID, payload.LookbackDays, fetch)
			status, message := "succeeded", fmt.Sprintf("已保存 %d 封邮件", saved)
			if err != nil {
				status, message = "failed", err.Error()
			}
			if recordErr := batch.record(account.ID, account.DisplayName+" · "+account.AppleID, status, message); recordErr != nil {
				return recordErr
			}
			if saved > 0 {
				hub.Publish("mailbox.changed", map[string]any{"account_id": account.ID, "messages_saved": saved})
			}
		}
		return batch.finish()
	}
}

func syncAccountMessages(ctx context.Context, store *repository.Store, accountID string, lookback int, fetch messagePageFetcher) (int, error) {
	connection, err := store.ConnectionSecret(ctx, accountID, domain.ConnectionIMAP)
	if err != nil {
		return 0, fmt.Errorf("尚未配置 IMAP 收件连接")
	}
	if connection.Status == domain.ConnectionDisabled {
		return 0, fmt.Errorf("IMAP 收件连接已停用")
	}
	var secret struct {
		Email       string `json:"email"`
		AppPassword string `json:"app_password"`
	}
	if err = json.Unmarshal([]byte(connection.SecretJSON), &secret); err != nil {
		return 0, err
	}
	if secret.Email == "" || secret.AppPassword == "" {
		return 0, fmt.Errorf("请先配置完整的 IMAP 收件凭据")
	}
	mailboxes, err := store.AllMailboxesByAccount(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if len(mailboxes) == 0 {
		return 0, nil
	}
	aliases := make([]protocol.Mailbox, 0, len(mailboxes))
	for _, m := range mailboxes {
		aliases = append(aliases, protocol.Mailbox{ID: m.ID, Email: m.Email})
	}
	stored, err := store.IMAPSyncState(ctx, accountID)
	if err != nil {
		return 0, err
	}
	signature := strings.ToLower(strings.TrimSpace(secret.Email))
	cursor := protocol.IMAPCursor{UIDValidity: stored.UIDValidity, LastUID: stored.LastUID}
	if stored.AccountSignature != signature || lookback > 0 {
		cursor = protocol.IMAPCursor{}
	}
	days := 30
	if value, err := store.Setting(ctx, "message_retention_days"); err == nil {
		if n, err := strconv.Atoi(value); err == nil && n >= 1 && n <= 365 {
			days = n
		}
	}
	if lookback > 0 {
		days = lookback
	}
	after := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	state := protocol.LoginState{Kind: protocol.LoginStateICloudIMAP, IMAPEmail: secret.Email, IMAPUsername: secret.Email, IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPAppPassword: secret.AppPassword}
	saved := 0
	for {
		result, err := fetch(ctx, state, aliases, after, cursor)
		if err != nil {
			return saved, err
		}
		if result.Cursor.UIDValidity == "" {
			return saved, fmt.Errorf("同步结果缺少收件箱代次，进度未更新")
		}
		inputs := []repository.MessageInput{}
		for id, rows := range result.MessagesByMailbox {
			for _, m := range rows {
				code := services.ExtractCode(m.Subject, m.Body)
				inputs = append(inputs, repository.MessageInput{MailboxID: id, RemoteID: m.RemoteID, Sender: m.From, Subject: m.Subject, Code: code.Code, LoginURL: m.LoginURL, MatchRule: code.Rule, Confidence: code.Confidence, Body: m.Body, ReceivedAt: m.ReceivedAt})
			}
		}
		n, err := store.SaveIMAPBatch(ctx, accountID, connection.SecretJSON, repository.IMAPSyncState{UIDValidity: result.Cursor.UIDValidity, LastUID: result.Cursor.LastUID, AccountSignature: signature}, inputs)
		if err != nil {
			return saved, err
		}
		saved += n
		if !result.HasMore {
			return saved, nil
		}
		if result.Cursor == cursor {
			return saved, fmt.Errorf("收件同步进度未向前推进")
		}
		cursor = result.Cursor
	}
}
