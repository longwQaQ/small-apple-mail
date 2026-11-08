package repository

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
)

func TestMailboxPurposeFilteringAndLeaseBoundary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, _ := s.CreateAccount(ctx, "pool@example.invalid", "", "")
	dedicated, purpose := "dedicated", "长期登录账号"
	private, err := s.CreateMailbox(ctx, account.ID, "private@example.invalid", "", "", []string{"Claude", "personal"}, MailboxPatch{AllocationMode: &dedicated, Purpose: &purpose})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.CreateMailbox(ctx, account.ID, "shared@example.invalid", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := s.Dashboard(ctx)
	if err != nil || dashboard["available_mailboxes"] != 1 {
		t.Fatal("可分配数量不应包含长期专用邮箱", dashboard["available_mailboxes"], err)
	}
	for _, filter := range []MailboxFilter{{Query: "claude"}, {Tag: "CLAUDE"}, {Mode: "dedicated"}, {Query: "长期登录"}} {
		rows, _, err := s.FilterMailboxes(ctx, filter)
		if err != nil || len(rows) != 1 || rows[0].ID != private.ID {
			t.Fatalf("filter=%+v rows=%+v err=%v", filter, rows, err)
		}
	}
	client, _, _ := s.CreateAPIClient(ctx, "test", []string{"leases:write", "codes:read"})
	_, _ = s.SaveMessages(ctx, []MessageInput{{MailboxID: shared.ID, RemoteID: "old", Code: "123456", ReceivedAt: time.Now().Add(-time.Minute)}})
	lease, box, _, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "one"})
	if err != nil || box.ID != shared.ID {
		t.Fatalf("专用邮箱不应分配: %s %v", box.ID, err)
	}
	if _, err = s.PatchMailbox(ctx, shared.ID, MailboxPatch{AllocationMode: &dedicated}); err == nil {
		t.Fatal("活动租约中不能修改用途")
	}
	mode, note := "shared", "租用中的邮箱仍可编辑备注"
	if updated, err := s.PatchMailbox(ctx, shared.ID, MailboxPatch{AllocationMode: &mode, Note: &note}); err != nil || updated.Note != note || updated.Status != domain.MailboxLeased {
		t.Fatalf("保留原用途类型时应允许编辑: %+v %v", updated, err)
	}
	if _, err = s.FindCode(ctx, lease, time.Now().Add(-time.Hour), "", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("新租约读取了历史验证码: %v", err)
	}
	_, _ = s.SaveMessages(ctx, []MessageInput{{MailboxID: shared.ID, RemoteID: "new", Code: "654321", ReceivedAt: time.Now()}})
	result, err := s.FindCode(ctx, lease, time.Now().Add(-time.Hour), "", true)
	if err != nil || result.Code != "654321" {
		t.Fatal(result, err)
	}
	if _, _, _, _, err = s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "two"}); !errors.Is(err, ErrPoolEmpty) {
		t.Fatal("只能使用长期专用邮箱时应报告池空", err)
	}
}

func TestV4MigrationPreservesDataAndPurposeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(legacyV4Schema()); err != nil {
		t.Fatal(err)
	}
	now := nowText()
	if _, err = db.Exec(`INSERT INTO apple_accounts(id,apple_id,created_at,updated_at) VALUES('a','test@example.invalid',?,?);INSERT INTO mailboxes(id,account_id,email,status,created_at,updated_at) VALUES('m','a','alias@example.invalid','available',?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Mailbox(ctx, "m")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if m.AllocationMode != "shared" {
				t.Fatal(m)
			}
			mode, purpose := "dedicated", "长期用途"
			if _, err = s.PatchMailbox(ctx, m.ID, MailboxPatch{AllocationMode: &mode, Purpose: &purpose}); err != nil {
				t.Fatal(err)
			}
		} else if m.AllocationMode != "dedicated" || m.Purpose != "长期用途" {
			t.Fatal(m)
		}
		if err = s.IntegrityCheck(ctx); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestResultBoundaryComparesTimestampsWithDifferentPrecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "boundary@example.invalid", "", "")
	m, _ := s.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	client, _, _ := s.CreateAPIClient(ctx, "boundary", []string{"leases:write", "codes:read"})
	lease, _, _, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "boundary"})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	lease.CreatedAt = base.Add(500 * time.Millisecond)
	if _, err = s.DB().ExecContext(ctx, "UPDATE leases SET created_at=? WHERE id=?", timeText(lease.CreatedAt), lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: m.ID, RemoteID: "old-second", Code: "111111", ReceivedAt: base}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FindCode(ctx, lease, base, "", true); !errors.Is(err, ErrNotFound) {
		t.Fatal("整数秒时间不应排在同秒的新租约之后", err)
	}
	if _, err = s.FindMailboxResult(ctx, m.ID, "code", "", lease.CreatedAt); !errors.Is(err, ErrNotFound) {
		t.Fatal("等待结果不应返回起始时间之前的邮件", err)
	}
	if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: m.ID, RemoteID: "new-fraction", Code: "222222", ReceivedAt: base.Add(750 * time.Millisecond)}}); err != nil {
		t.Fatal(err)
	}
	if result, err := s.FindCode(ctx, lease, base, "", true); err != nil || result.Code != "222222" {
		t.Fatal(result, err)
	}
	if result, err := s.FindMailboxResult(ctx, m.ID, "code", "", base.Add(-time.Second)); err != nil || result.Code != "222222" {
		t.Fatal("应按时间而非字符串顺序选择最新结果", result, err)
	}
}

func TestRemoteCreationStoresPurposeBeforeAllocation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "remote@example.invalid", "", "")
	m, err := s.SaveCreatedRemoteMailbox(ctx, a.ID, RemoteMailboxInput{Email: "alias@example.invalid", AnonymousID: "remote", Active: true, AllocationMode: "dedicated", Purpose: "专用"})
	if err != nil || m.AllocationMode != "dedicated" || m.Purpose != "专用" {
		t.Fatal(m, err)
	}
	if _, err = s.SyncRemoteMailboxes(ctx, a.ID, []RemoteMailboxInput{{Email: m.Email, AnonymousID: "remote", Active: true}}); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mailbox(ctx, m.ID)
	if m.AllocationMode != "dedicated" {
		t.Fatal("远端同步覆盖了本地用途")
	}
}

func TestCodeBoundaryRetainsNanosecondPrecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "nano@example.invalid", "", "")
	m, _ := s.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	client, _, _ := s.CreateAPIClient(ctx, "nano", []string{"leases:write", "codes:read"})
	lease, _, _, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	lease.CreatedAt = time.Now().UTC().Add(-time.Second).Truncate(time.Second).Add(500000001 * time.Nanosecond)
	if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: m.ID, RemoteID: "old", Code: "111111", ReceivedAt: lease.CreatedAt.Add(-time.Nanosecond)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FindCode(ctx, lease, lease.CreatedAt.Add(-time.Hour), "", true); !errors.Is(err, ErrNotFound) {
		t.Fatal("旧验证码与租约仅差 1 纳秒时也不能越界", err)
	}
	if _, err = s.FindMailboxResult(ctx, m.ID, "code", "", lease.CreatedAt); !errors.Is(err, ErrNotFound) {
		t.Fatal("等待查询不能因时间精度损失返回旧结果", err)
	}
	if _, err = s.FindMailboxCode(ctx, m.ID, lease.CreatedAt, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("单邮箱取码应遵守相同边界", err)
	}
	if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: m.ID, RemoteID: "new", Code: "222222", ReceivedAt: lease.CreatedAt}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.FindCode(ctx, lease, lease.CreatedAt.Add(-time.Hour), "", true)
	if err != nil || result.Code != "222222" {
		t.Fatal(result, err)
	}
	boxes, _, err := s.ListMailboxes(ctx, "", "", a.ID, "", 10)
	if err != nil || len(boxes) != 1 || boxes[0].LastCode != "222222" {
		t.Fatal("列表应按完整时间精度显示最新结果", boxes, err)
	}
	rows, err := s.ListMessages(ctx, m.ID, 10)
	if err != nil || len(rows) != 2 || rows[0].Code != "222222" {
		t.Fatal("收件列表应使用相同排序", rows, err)
	}
}

func TestIMAPBatchCursorAndUIDGenerationAreAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "imap@example.invalid", "", "")
	m, _ := s.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	_, _ = s.UpsertConnection(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "", "secret-one", "")
	first := IMAPSyncState{UIDValidity: "10", LastUID: "1", AccountSignature: "owner"}
	input := []MessageInput{{MailboxID: m.ID, RemoteID: "imap:1", Code: "111111", ReceivedAt: time.Now()}}
	if _, err := s.SaveIMAPBatch(ctx, a.ID, "secret-one", first, input); err != nil {
		t.Fatal(err)
	}
	first.UIDValidity = "20"
	input[0].Code = "222222"
	if _, err := s.SaveIMAPBatch(ctx, a.ID, "secret-one", first, input); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ListMessages(ctx, m.ID, 10)
	if len(rows) != 2 {
		t.Fatal("UID 代次变化不应覆盖历史邮件", rows)
	}
	ids, _ := s.RemoteVerificationMessageIDs(ctx, a.ID)
	if len(ids) != 1 || ids[0] != "imap:1" {
		t.Fatal("历史代次不应再参与远端清理", ids)
	}
	_, _ = s.UpsertConnection(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "", "secret-two", "")
	first.LastUID = "2"
	input[0].RemoteID = "imap:2"
	if _, err := s.SaveIMAPBatch(ctx, a.ID, "secret-one", first, input); err == nil {
		t.Fatal("旧连接不应提交同步结果")
	}
	cursor, _ := s.IMAPSyncState(ctx, a.ID)
	if cursor.LastUID != "1" {
		t.Fatal("失败提交更新了游标", cursor)
	}
	rows, _ = s.ListMessages(ctx, m.ID, 10)
	if len(rows) != 2 {
		t.Fatal("失败提交写入了邮件")
	}
}

func TestFirstIMAPSyncSeparatesLegacyUIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "legacy@example.invalid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMailbox(ctx, a.ID, "alias@example.invalid", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = `{"email":"owner@example.invalid","app_password":"fixture"}`
	if _, err = s.UpsertConnection(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "", secret, ""); err != nil {
		t.Fatal(err)
	}
	// 旧版邮件没有记录 UIDVALIDITY，不能认定与首次同步属于同一代次。
	if _, err = s.SaveMessages(ctx, []MessageInput{{MailboxID: m.ID, RemoteID: "imap:1", Code: "111111", Subject: "旧验证码", ReceivedAt: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	state := IMAPSyncState{UIDValidity: "20", LastUID: "1", AccountSignature: "owner@example.invalid"}
	const link = "https://claude.ai/fixture-login-link"
	if n, err := s.SaveIMAPBatch(ctx, a.ID, secret, state, []MessageInput{{MailboxID: m.ID, RemoteID: "imap:1", LoginURL: link, Subject: "新登录邮件", ReceivedAt: time.Now()}}); err != nil || n != 1 {
		t.Fatal("首次同步未保存当前代次邮件", n, err)
	}
	rows, err := s.ListMessages(ctx, m.ID, 10)
	if err != nil || len(rows) != 2 {
		t.Fatal("旧邮件应单独保留", rows, err)
	}
	for _, row := range rows {
		if row.Code != "" && row.LoginURL != "" {
			t.Fatal("新登录链接被合并到旧验证码邮件")
		}
	}
	ids, err := s.RemoteVerificationMessageIDs(ctx, a.ID)
	if err != nil || len(ids) != 0 {
		t.Fatal("未知代次的旧邮件不应参与远端清理", ids, err)
	}
	if n, err := s.SaveIMAPBatch(ctx, a.ID, secret, state, nil); err != nil || n != 0 {
		t.Fatal(err)
	}
	rows, err = s.ListMessages(ctx, m.ID, 10)
	if err != nil || len(rows) != 2 {
		t.Fatal("后续同步不应重复归档", rows, err)
	}
}

func TestIMAPNotificationsKeepOneTrailingSync(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "queue@example.invalid", "", "")
	first, created, err := s.RequestMessageSyncJob(ctx, a.ID)
	if err != nil || !created {
		t.Fatal(err)
	}
	claimed, err := s.ClaimJob(ctx)
	if err != nil || claimed.ID != first.ID {
		t.Fatal(err)
	}
	next, created, err := s.RequestMessageSyncJob(ctx, a.ID)
	if err != nil || !created || next.ID == first.ID {
		t.Fatal(next, err)
	}
	again, created, err := s.RequestMessageSyncJob(ctx, a.ID)
	if err != nil || created || again.ID != next.ID {
		t.Fatal(again, err)
	}
	if _, err = s.ClaimJob(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("收件同步不能并发推进游标", err)
	}
	if err = s.CompleteJob(ctx, first.ID, domain.JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimJob(ctx)
	if err != nil || claimed.ID != next.ID {
		t.Fatal("后续通知未执行", claimed, err)
	}
}

func TestMessageSyncAndCleanupCannotRunConcurrently(t *testing.T) {
	for _, activeType := range []string{"mailbox.message_sync", "mailbox.remote_clean"} {
		t.Run(activeType, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			first, _ := s.CreateJob(ctx, activeType, "{}", 20)
			if job, err := s.ClaimJob(ctx); err != nil || job.ID != first.ID {
				t.Fatal(job, err)
			}
			_, _ = s.CreateJob(ctx, "mailbox.message_sync", "{}", 10)
			_, _ = s.CreateJob(ctx, "mailbox.remote_clean", "{}", 10)
			other, _ := s.CreateJob(ctx, "database.integrity_check", "{}", 0)
			if job, err := s.ClaimJob(ctx); err != nil || job.ID != other.ID {
				t.Fatal("UID 操作需等待，其他任务仍可执行", job, err)
			}
			if _, err := s.ClaimJob(ctx); !errors.Is(err, ErrNotFound) {
				t.Fatal("不应同时领取另一个 UID 操作", err)
			}
			if err := s.CompleteJob(ctx, first.ID, domain.JobSucceeded, "", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ClaimJob(ctx); err != nil {
				t.Fatal("前一任务完成后应允许继续", err)
			}
		})
	}
}
