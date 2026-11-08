package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/events"
	"small-apple-mail/internal/app/protocol"
	"small-apple-mail/internal/app/repository"
	"small-apple-mail/internal/app/worker"
)

func Register(pool *worker.Pool, store *repository.Store, hub *events.Hub) *ConnectionService {
	connections := newConnectionService(store, hub)
	pool.Register("account.connection.check", connections.checkJob)
	pool.Register("mailbox.remote_sync", connections.remoteSyncJob())
	pool.Register("mailbox.create", connections.createMailboxesJob())
	pool.Register("mailbox.message_sync", messageSyncHandler(store, hub, protocol.SyncICloudIMAPPage))
	pool.Register("message.retention_cleanup", func(ctx context.Context, _ domain.Job) error {
		removed, err := store.CleanupMessages(ctx)
		if err == nil {
			hub.Publish("system.notice", map[string]any{"message": "消息留存清理完成", "removed": removed})
		}
		return err
	})
	pool.Register("account.keepalive", connections.keepAliveJob)
	pool.Register("mailbox.remote_clean", func(ctx context.Context, job domain.Job) error {
		var payload struct {
			AccountIDs []string `json:"account_ids"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return err
		}
		wanted := map[string]bool{}
		for _, id := range payload.AccountIDs {
			wanted[id] = true
		}
		accounts, err := store.ListAccounts(ctx)
		if err != nil {
			return err
		}
		cleaned := 0
		for _, account := range accounts {
			if len(wanted) > 0 && !wanted[account.ID] {
				continue
			}
			ids, err := store.RemoteVerificationMessageIDs(ctx, account.ID)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				continue
			}
			connection, err := store.ConnectionSecret(ctx, account.ID, domain.ConnectionIMAP)
			if err != nil {
				continue
			}
			var secret struct {
				Email       string `json:"email"`
				AppPassword string `json:"app_password"`
			}
			if err = json.Unmarshal([]byte(connection.SecretJSON), &secret); err != nil {
				return err
			}
			cursor, err := store.IMAPSyncState(ctx, account.ID)
			if err != nil {
				return err
			}
			if cursor.UIDValidity == "" || cursor.AccountSignature != strings.ToLower(strings.TrimSpace(secret.Email)) {
				return fmt.Errorf("账号 %s 的收件连接尚未同步，请先同步收件后再清理", account.AppleID)
			}
			state := protocol.LoginState{Kind: protocol.LoginStateICloudIMAP, IMAPEmail: secret.Email, IMAPUsername: secret.Email, IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPAppPassword: secret.AppPassword, IMAPUIDValidity: cursor.UIDValidity}
			result, err := protocol.MoveICloudIMAPMessagesToTrash(ctx, state, ids)
			if err != nil {
				return err
			}
			if err = store.MarkRemoteVerificationMessagesCleaned(ctx, account.ID, ids); err != nil {
				return err
			}
			cleaned += result.MovedToTrash
		}
		hub.Publish("system.notice", map[string]any{"message": "远端验证码邮件已移至废纸篓", "cleaned": cleaned})
		return nil
	})
	return connections
}

func connectionFailureMessage(err error) string {
	const (
		prefix   = "连接检测失败"
		maxRunes = 300
	)
	if err == nil {
		return prefix
	}
	detail := strings.TrimSpace(err.Error())
	if detail == "" {
		return prefix
	}
	message := []rune(prefix + "：" + detail)
	if len(message) <= maxRunes {
		return string(message)
	}
	return string(message[:maxRunes-1]) + "…"
}
