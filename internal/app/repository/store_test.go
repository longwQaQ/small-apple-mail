package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "small-apple-mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInitialSchemaBootstrapAndIntegrity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.BootstrapAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapAdmin(ctx, "ignored", "another valid password"); err != nil {
		t.Fatal(err)
	}
	admin, err := s.Admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Username != "admin" {
		t.Fatalf("username=%q", admin.Username)
	}
	if err = s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var appID string
	if err = s.DB().QueryRowContext(ctx, "SELECT value FROM application_meta WHERE key='application_id'").Scan(&appID); err != nil || appID != applicationID {
		t.Fatalf("application_id=%q err=%v", appID, err)
	}
}

func TestBootstrapAdminRejectsPasswordShorterThanEightCharacters(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.BootstrapAdmin(ctx, "admin", "1234567"); err == nil {
		t.Fatal("应拒绝少于 8 个字符的管理员密码")
	}
	if _, err := s.Admin(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("失败的初始化不应创建管理员: %v", err)
	}
}

func TestAllMailboxesAndUpdateBeyondFirstPage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "many@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if _, err = s.CreateMailbox(ctx, account.ID, fmt.Sprintf("alias-%03d@icloud.com", i), "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.AllMailboxesByAccount(ctx, account.ID)
	if err != nil || len(all) != 205 {
		t.Fatalf("all=%d err=%v", len(all), err)
	}
	firstPage, _, err := s.ListMailboxes(ctx, "", "", account.ID, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	visible := make(map[string]bool, len(firstPage))
	for _, mailbox := range firstPage {
		visible[mailbox.ID] = true
	}
	var beyond domain.Mailbox
	for _, mailbox := range all {
		if !visible[mailbox.ID] {
			beyond = mailbox
			break
		}
	}
	if beyond.ID == "" {
		t.Fatal("没有找到第一页之外的邮箱")
	}
	label := "分页外已更新"
	updated, err := s.UpdateMailbox(ctx, beyond.ID, &label, nil, nil, nil, nil)
	if err != nil || updated.ID != beyond.ID || updated.Label != label {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestOpenRejectsUnknownExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec("DROP TABLE application_meta"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(context.Background(), path); err == nil {
		t.Fatal("应拒绝缺少 application_meta 的已有数据库")
	}
}

func TestOpenMigratesV1MessageBodies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v1Schema := strings.Replace(legacyV3Schema(), "('schema_version','3')", "('schema_version','1')", 1)
	v1Schema = strings.Replace(v1Schema, " login_url TEXT NOT NULL DEFAULT '',", "", 1)
	v1Schema = strings.Replace(v1Schema, "confidence REAL NOT NULL DEFAULT 0 CHECK(confidence>=0 AND confidence<=1), body TEXT NOT NULL DEFAULT '',", "confidence REAL NOT NULL DEFAULT 0 CHECK(confidence>=0 AND confidence<=1), debug_body TEXT NOT NULL DEFAULT '', debug_body_expires_at TEXT,", 1)
	if _, err = db.ExecContext(ctx, v1Schema); err != nil {
		t.Fatal(err)
	}
	now := nowText()
	if _, err = db.ExecContext(ctx, `INSERT INTO apple_accounts(id,apple_id,status,created_at,updated_at) VALUES('acc','v1@icloud.com','healthy',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO mailboxes(id,account_id,email,status,created_at,updated_at) VALUES('mbx','acc','box@icloud.com','available',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,mailbox_id,remote_id,debug_body,received_at,synced_at) VALUES('msg','mbx','remote','历史正文',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	message, err := s.Message(ctx, "mbx", "msg")
	if err != nil || message.Body != "历史正文" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	if version, err := s.SchemaVersion(ctx); err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestSQLitePragmasApplyToEveryConnection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	connections := make([]*sql.Conn, 0, 6)
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for i := 0; i < 6; i++ {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
		var foreignKeys, busyTimeout int
		var journalMode, synchronous string
		if err = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" || synchronous != "1" {
			t.Fatalf("connection %d pragmas: fk=%d busy=%d journal=%s sync=%s", i, foreignKeys, busyTimeout, journalMode, synchronous)
		}
	}
}

func TestDashboardConnectionGuidanceCounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "guide@icloud.com", "Guide", "")
	if err != nil {
		t.Fatal(err)
	}
	values, err := s.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if values["accounts_without_create_connection"] != 1 || values["accounts_without_imap"] != 1 {
		t.Fatalf("initial guidance=%+v", values)
	}
	if _, err = s.UpsertConnection(ctx, account.ID, domain.ConnectionAppleAccount, domain.ConnectionHealthy, "", "{}", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpsertConnection(ctx, account.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "imap.mail.me.com", "{}", ""); err != nil {
		t.Fatal(err)
	}
	values, err = s.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if values["accounts_without_create_connection"] != 0 || values["accounts_without_imap"] != 0 {
		t.Fatalf("configured guidance=%+v", values)
	}
}

func TestJobPaginationAndTwentyFourHourRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if _, err := s.CreateJob(ctx, "database.integrity_check", "{}", 0); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := s.ListJobsPage(ctx, "", time.Now().UTC().Add(-24*time.Hour), 20)
	if err != nil || len(first) != 20 || next == "" {
		t.Fatalf("first=%d next=%q err=%v", len(first), next, err)
	}
	second, next, err := s.ListJobsPage(ctx, next, time.Now().UTC().Add(-24*time.Hour), 20)
	if err != nil || len(second) != 5 || next != "" {
		t.Fatalf("second=%d next=%q err=%v", len(second), next, err)
	}

	oldFinished, err := s.CreateJob(ctx, "database.integrity_check", "{}", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteJob(ctx, oldFinished.ID, domain.JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddJobEvent(ctx, oldFinished.ID, "done", "info", "done", "{}"); err != nil {
		t.Fatal(err)
	}
	oldActive, err := s.CreateJob(ctx, "mailbox.create", "{}", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := timeText(time.Now().UTC().Add(-25 * time.Hour))
	if _, err = s.DB().ExecContext(ctx, "UPDATE jobs SET created_at=? WHERE id IN (?,?)", oldTime, oldFinished.ID, oldActive.ID); err != nil {
		t.Fatal(err)
	}
	removed, err := s.DeleteJobsBefore(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	var finishedCount, eventCount, activeCount int
	_ = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE id=?", oldFinished.ID).Scan(&finishedCount)
	_ = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM job_events WHERE job_id=?", oldFinished.ID).Scan(&eventCount)
	_ = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE id=?", oldActive.ID).Scan(&activeCount)
	if finishedCount != 0 || eventCount != 0 || activeCount != 1 {
		t.Fatalf("finished=%d events=%d active=%d", finishedCount, eventCount, activeCount)
	}
}

func TestScheduleCRUDAndManualResume(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	created, err := s.CreateSchedule(ctx, "account.connection.check", " 每小时检测 ", `{"account_ids":["acc_demo"]}`, false, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "每小时检测" || created.Enabled || created.Type != "account.connection.check" {
		t.Fatalf("created=%+v", created)
	}

	loaded, err := s.Schedule(ctx, created.ID)
	if err != nil || loaded.Payload != `{"account_ids":["acc_demo"]}` {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE schedules SET consecutive_failures=3,paused_reason='连续失败' WHERE id=?", created.ID); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateSchedule(ctx, created.ID, "database.integrity_check", "数据库检查", `{}`, true, 7200)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.Type != "database.integrity_check" || updated.IntervalSeconds != 7200 || updated.ConsecutiveFailures != 0 || updated.PausedReason != "" {
		t.Fatalf("updated=%+v", updated)
	}

	job, err := s.RunScheduleNow(ctx, created.ID)
	if err != nil || job.Type != "database.integrity_check" || job.Payload != `{}` {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err = s.DeleteSchedule(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Schedule(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted schedule err=%v", err)
	}

	invalid := []struct {
		name     string
		payload  string
		interval int
	}{
		{"", `{}`, 3600},
		{"无效参数", `[]`, 3600},
		{"间隔过短", `{}`, 59},
		{"间隔过长", `{}`, 365*24*60*60 + 1},
	}
	for _, item := range invalid {
		if _, err = s.CreateSchedule(ctx, "database.integrity_check", item.name, item.payload, true, item.interval); err == nil {
			t.Fatalf("expected validation error for %+v", item)
		}
	}
}

func TestScheduledJobCompletionUpdatesFailureState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	schedule, err := s.CreateSchedule(ctx, "database.integrity_check", "数据库检查", `{}`, true, 3600)
	if err != nil {
		t.Fatal(err)
	}
	completeScheduledJob := func(status string) {
		t.Helper()
		job, createErr := s.CreateJob(ctx, schedule.Type, schedule.Payload, 0)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, createErr = s.DB().ExecContext(ctx, "UPDATE jobs SET schedule_id=? WHERE id=?", schedule.ID, job.ID); createErr != nil {
			t.Fatal(createErr)
		}
		if createErr = s.CompleteJob(ctx, job.ID, status, "", ""); createErr != nil {
			t.Fatal(createErr)
		}
	}
	for i := 1; i <= 3; i++ {
		completeScheduledJob(domain.JobFailed)
		loaded, loadErr := s.Schedule(ctx, schedule.ID)
		if loadErr != nil || loaded.ConsecutiveFailures != i {
			t.Fatalf("failure=%d schedule=%+v err=%v", i, loaded, loadErr)
		}
		if i < 3 && loaded.PausedReason != "" {
			t.Fatalf("第 %d 次失败不应暂停: %+v", i, loaded)
		}
		if i == 3 && loaded.PausedReason == "" {
			t.Fatalf("连续三次失败后应自动暂停: %+v", loaded)
		}
	}
	completeScheduledJob(domain.JobSucceeded)
	loaded, err := s.Schedule(ctx, schedule.ID)
	if err != nil || loaded.ConsecutiveFailures != 0 || loaded.PausedReason != "" {
		t.Fatalf("成功后应清除失败状态: %+v err=%v", loaded, err)
	}
}

func TestEnsureMessageSyncJobDeduplicatesActiveAccountJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, created, err := s.EnsureMessageSyncJob(ctx, "acc-one", 15)
	if err != nil || !created {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	second, created, err := s.EnsureMessageSyncJob(ctx, "acc-one", 15)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second=%+v created=%v err=%v", second, created, err)
	}
	other, created, err := s.EnsureMessageSyncJob(ctx, "acc-two", 15)
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("other=%+v created=%v err=%v", other, created, err)
	}
	if err = s.CompleteJob(ctx, first.ID, domain.JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	third, created, err := s.EnsureMessageSyncJob(ctx, "acc-one", 15)
	if err != nil || !created || third.ID == first.ID {
		t.Fatalf("third=%+v created=%v err=%v", third, created, err)
	}
}

func TestScheduleDoesNotEnqueueDuplicateUnfinishedJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	schedule, err := s.CreateSchedule(ctx, "database.integrity_check", "去重检查", `{}`, true, 60)
	if err != nil {
		t.Fatal(err)
	}
	makeDue := func() {
		if _, updateErr := s.DB().ExecContext(ctx, "UPDATE schedules SET next_run_at=? WHERE id=?", timeText(time.Now().UTC().Add(-time.Minute)), schedule.ID); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	makeDue()
	first, err := s.EnqueueDueSchedules(ctx)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%d err=%v", len(first), err)
	}
	makeDue()
	duplicate, err := s.EnqueueDueSchedules(ctx)
	if err != nil || len(duplicate) != 0 {
		t.Fatalf("duplicate=%d err=%v", len(duplicate), err)
	}
	if err = s.CompleteJob(ctx, first[0].ID, domain.JobSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	makeDue()
	next, err := s.EnqueueDueSchedules(ctx)
	if err != nil || len(next) != 1 {
		t.Fatalf("next=%d err=%v", len(next), err)
	}
}

func TestConnectionCredentialsRemainPlaintextButAreNotSerialized(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "plain@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	secret := `{"email":"plain@icloud.com","app_password":"abcd-efgh-ijkl-mnop"}`
	connection, err := s.UpsertConnection(ctx, account.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "imap.mail.me.com", secret, "")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = s.DB().QueryRowContext(ctx, "SELECT secret_json FROM account_connections WHERE id=?", connection.ID).Scan(&stored); err != nil || stored != secret {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
	encoded, err := json.Marshal(connection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "app_password") || strings.Contains(string(encoded), "abcd-efgh") {
		t.Fatalf("API 模型泄露连接凭据: %s", encoded)
	}
}

func TestMailboxAPITokenRotationAndLookup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, _ := s.CreateAccount(ctx, "mailbox-api@icloud.com", "", "")
	mailbox, _ := s.CreateMailbox(ctx, account.ID, "api@icloud.com", "", "", nil)
	first, err := s.RotateMailboxAPIToken(ctx, mailbox.ID)
	if err != nil || !strings.HasPrefix(first, "sam_mailbox_") {
		t.Fatalf("first token err=%v", err)
	}
	if _, err = s.MailboxByAPIToken(ctx, mailbox.ID, first); err != nil {
		t.Fatalf("verify first: %v", err)
	}
	second, err := s.RotateMailboxAPIToken(ctx, mailbox.ID)
	if err != nil || second == "" || second == first {
		t.Fatalf("second token err=%v", err)
	}
	if _, err = s.MailboxByAPIToken(ctx, mailbox.ID, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token err=%v", err)
	}
	if _, err = s.MailboxByAPIToken(ctx, mailbox.ID, second); err != nil {
		t.Fatalf("verify second: %v", err)
	}
	var stored string
	if err = s.DB().QueryRowContext(ctx, "SELECT token_hash FROM mailbox_api_credentials WHERE mailbox_id=?", mailbox.ID).Scan(&stored); err != nil || stored == second {
		t.Fatalf("stored token is not hashed: %q err=%v", stored, err)
	}
	if err = s.DeleteMailbox(ctx, mailbox.ID); err != nil {
		t.Fatalf("delete mailbox with api credential: %v", err)
	}
	var credentials int
	_ = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM mailbox_api_credentials WHERE mailbox_id=?", mailbox.ID).Scan(&credentials)
	if credentials != 0 {
		t.Fatalf("credentials=%d", credentials)
	}
}

func TestConcurrentLeaseNeverDuplicatesMailbox(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "owner@icloud.com", "Owner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateMailbox(ctx, account.ID, "one@icloud.com", "", "", nil); err != nil {
		t.Fatal(err)
	}
	client, key, err := s.CreateAPIClient(ctx, "test", []string{"leases:read", "leases:write", "codes:read"})
	if err != nil || key == "" {
		t.Fatal(err)
	}
	const workers = 12
	var wg sync.WaitGroup
	wg.Add(workers)
	success := 0
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, _, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: time.Now().String() + string(rune('a'+i))})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
				return
			}
			if !errors.Is(err, ErrPoolEmpty) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("success=%d want 1", success)
	}
	var active int
	if err = s.DB().QueryRow("SELECT COUNT(*) FROM leases WHERE status=?", domain.LeaseActive).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}

func TestLeaseIdempotencyAndConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, _ := s.CreateAccount(ctx, "owner@icloud.com", "", "")
	_, _ = s.CreateMailbox(ctx, account.ID, "one@icloud.com", "", "", nil)
	client, _, _ := s.CreateAPIClient(ctx, "client", []string{"leases:write"})
	req := LeaseRequest{Project: "p", TTLSeconds: 1800, IdempotencyKey: "same"}
	first, _, token, replay, err := s.CreateLease(ctx, client, req)
	if err != nil || replay || token == "" {
		t.Fatalf("first replay=%v err=%v", replay, err)
	}
	second, _, token2, replay, err := s.CreateLease(ctx, client, req)
	if err != nil || !replay || first.ID != second.ID || token != token2 {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	req.Purpose = "different"
	if _, _, _, _, err = s.CreateLease(ctx, client, req); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestDeleteAPIClientCascadesInactiveLeaseHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, _ := s.CreateAccount(ctx, "delete-client@icloud.com", "", "")
	_, _ = s.CreateMailbox(ctx, account.ID, "delete-client-alias@icloud.com", "", "", nil)
	client, _, err := s.CreateAPIClient(ctx, "delete-client", []string{"leases:write"})
	if err != nil {
		t.Fatal(err)
	}
	lease, _, token, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "delete-client-lease"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseLease(ctx, client.ID, lease.ID, token, domain.LeaseReleased); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteAPIClient(ctx, client.ID); err != nil {
		t.Fatalf("删除只有历史租约的客户端失败: %v", err)
	}
	var leases int
	if err = s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE api_client_id=?", client.ID).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("leases=%d err=%v", leases, err)
	}
}

func TestLeaseReleaseAndExpiryRestoreRemoteMissingStatus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, _ := s.CreateAccount(ctx, "missing@icloud.com", "", "")
	firstMailbox, _ := s.CreateMailbox(ctx, account.ID, "missing-release@icloud.com", "", "", nil)
	secondMailbox, _ := s.CreateMailbox(ctx, account.ID, "missing-expire@icloud.com", "", "", nil)
	client, _, _ := s.CreateAPIClient(ctx, "missing-client", []string{"leases:write"})
	firstLease, leasedMailbox, firstToken, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "missing-release"})
	if err != nil {
		t.Fatal(err)
	}
	if leasedMailbox.ID != firstMailbox.ID && leasedMailbox.ID != secondMailbox.ID {
		t.Fatalf("unexpected mailbox: %+v", leasedMailbox)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE mailboxes SET remote_present=0,remote_status='missing' WHERE id=?", leasedMailbox.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseLease(ctx, client.ID, firstLease.ID, firstToken, domain.LeaseReleased); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Mailbox(ctx, leasedMailbox.ID)
	if err != nil || loaded.Status != domain.MailboxMissing {
		t.Fatalf("released mailbox=%+v err=%v", loaded, err)
	}
	secondLease, secondLeasedMailbox, _, _, err := s.CreateLease(ctx, client, LeaseRequest{IdempotencyKey: "missing-expire"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE mailboxes SET remote_present=0,remote_status='missing' WHERE id=?", secondLeasedMailbox.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE leases SET expires_at=? WHERE id=?", timeText(time.Now().UTC().Add(-time.Minute)), secondLease.ID); err != nil {
		t.Fatal(err)
	}
	if expired, expireErr := s.ExpireLeases(ctx); expireErr != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, expireErr)
	}
	loaded, err = s.Mailbox(ctx, secondLeasedMailbox.ID)
	if err != nil || loaded.Status != domain.MailboxMissing {
		t.Fatalf("expired mailbox=%+v err=%v", loaded, err)
	}
}

func TestActivityPaginationAndCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if err := s.CreateActivity(ctx, domain.Activity{Source: "manual", Type: "manual.note", Result: "success", Detail: "测试活动"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.ListActivitiesPage(ctx, 2, 20)
	if err != nil || total != 25 || len(rows) != 5 {
		t.Fatalf("total=%d rows=%d err=%v", total, len(rows), err)
	}
	if rows[0].Source != "manual" {
		t.Fatalf("source=%q", rows[0].Source)
	}
	id := rows[0].ID
	if err = s.UpdateActivity(ctx, id, "manual.updated", "failed", "更新后的活动"); err != nil {
		t.Fatal(err)
	}
	page, _, err := s.ListActivitiesPage(ctx, 2, 20)
	if err != nil || page[0].Type != "manual.updated" || page[0].Result != "failed" {
		t.Fatalf("updated=%+v err=%v", page[0], err)
	}
	if err = s.DeleteActivity(ctx, id); err != nil {
		t.Fatal(err)
	}
	_, total, err = s.ListActivitiesPage(ctx, 1, 20)
	if err != nil || total != 24 {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestDeleteAllJobs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.CreateJob(ctx, "database.integrity_check", "{}", 0); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.DeleteAllJobs(ctx)
	if err != nil || deleted != 3 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	jobs, err := s.ListJobs(ctx, 20)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
}

func TestAdminSessionAccountMailboxAndClientLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.BootstrapAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	admin, err := s.AdminByUsername(ctx, "ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkAdminLogin(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	session, token, csrf, err := s.CreateSession(ctx, time.Hour, 24*time.Hour)
	if err != nil || token == "" || csrf == "" {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	loadedSession, err := s.SessionByToken(ctx, token, time.Hour)
	if err != nil || loadedSession.ID != session.ID {
		t.Fatalf("loaded=%+v err=%v", loadedSession, err)
	}
	if err = s.ChangePassword(ctx, admin.ID, admin.PasswordHash, session.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SessionByToken(ctx, token, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session err=%v", err)
	}

	account, err := s.CreateAccount(ctx, "lifecycle@icloud.com", "原名称", "原备注")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Account(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	updatedAccount, err := s.UpdateAccount(ctx, account.ID, "新名称", "新备注")
	if err != nil || updatedAccount.DisplayName != "新名称" {
		t.Fatalf("account=%+v err=%v", updatedAccount, err)
	}
	if _, err = s.UpsertConnection(ctx, account.ID, domain.ConnectionICloudWeb, domain.ConnectionFailed, "www.icloud.com", `{}`, "失败"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAccountComputedStatus(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SetConnectionStatus(ctx, account.ID, domain.ConnectionICloudWeb, domain.ConnectionHealthy, "正常"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConnectionSecret(ctx, account.ID, domain.ConnectionICloudWeb); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteConnection(ctx, account.ID, domain.ConnectionICloudWeb); err != nil {
		t.Fatal(err)
	}

	mailbox, err := s.CreateMailbox(ctx, account.ID, "lifecycle@icloud.com", "标签", "备注", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	label, note, enabled, tags := "新标签", "新备注", false, `["two"]`
	updatedMailbox, err := s.UpdateMailbox(ctx, mailbox.ID, &label, &note, nil, &enabled, &tags)
	if err != nil || updatedMailbox.Label != label || updatedMailbox.LocalEnabled {
		t.Fatalf("mailbox=%+v err=%v", updatedMailbox, err)
	}
	if _, err = s.Mailbox(ctx, mailbox.ID); err != nil {
		t.Fatal(err)
	}
	impact, err := s.AccountImpact(ctx, account.ID)
	if err != nil || impact["mailboxes"] != 1 {
		t.Fatalf("impact=%v err=%v", impact, err)
	}

	client, key, err := s.CreateAPIClient(ctx, "lifecycle-client", []string{"leases:read", "codes:read"})
	if err != nil || key == "" {
		t.Fatal(err)
	}
	if loaded, err := s.APIClientByKey(ctx, key); err != nil || loaded.ID != client.ID || !HasScope(loaded, "codes:read") {
		t.Fatalf("client=%+v err=%v", loaded, err)
	}
	newKey, err := s.RotateAPIClient(ctx, client.ID)
	if err != nil || newKey == key {
		t.Fatalf("newKey=%q err=%v", newKey, err)
	}
	if err = s.SetAPIClientStatus(ctx, client.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	clients, err := s.ListAPIClients(ctx)
	if err != nil || len(clients) != 1 || clients[0].Status != "disabled" {
		t.Fatalf("clients=%+v err=%v", clients, err)
	}
	if err = s.DeleteAPIClient(ctx, client.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteMailbox(ctx, mailbox.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteMessagesAndLeaseLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	account, err := s.CreateAccount(ctx, "remote@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.SaveCreatedRemoteMailbox(ctx, account.ID, RemoteMailboxInput{AnonymousID: "remote-one", Email: "one@icloud.com", Label: "One", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if count, err := s.SyncRemoteMailboxesNoMissing(ctx, account.ID, []RemoteMailboxInput{{AnonymousID: "remote-two", Email: "two@icloud.com", Active: true}}); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if count, err := s.SyncRemoteMailboxes(ctx, account.ID, []RemoteMailboxInput{{AnonymousID: "remote-one", Email: "one@icloud.com", Active: true}}); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	rows, next, err := s.ListMailboxes(ctx, "one", domain.MailboxAvailable, account.ID, "", 1)
	if err != nil || len(rows) != 1 || next != "" || rows[0].ID != created.ID {
		t.Fatalf("rows=%+v next=%q err=%v", rows, next, err)
	}

	now := time.Now().UTC()
	saved, err := s.SaveMessages(ctx, []MessageInput{
		{MailboxID: created.ID, RemoteID: "imap:1", Sender: "OpenAI", Subject: "Code", Code: "123456", MatchRule: "otp", Confidence: 1, Body: "sensitive body", ReceivedAt: now},
		{MailboxID: created.ID, RemoteID: "imap:2", Sender: "Newsletter", Subject: "Regular", ReceivedAt: now.Add(-40 * 24 * time.Hour)},
	})
	if err != nil || saved != 2 {
		t.Fatalf("saved=%d err=%v", saved, err)
	}
	messages, err := s.ListMessages(ctx, created.ID, 10)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
	if message, err := s.Message(ctx, created.ID, messages[0].ID); err != nil || message.Body != "sensitive body" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	if mailboxes, _, err := s.ListMailboxes(ctx, "one", "", account.ID, "", 10); err != nil || len(mailboxes) != 1 || mailboxes[0].LastCode != "123456" || !mailboxes[0].LastCodeAt.Equal(now) {
		t.Fatalf("mailboxes=%+v err=%v", mailboxes, err)
	}
	if ids, err := s.RemoteVerificationMessageIDs(ctx, account.ID); err != nil || len(ids) != 1 || ids[0] != "imap:1" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if err = s.MarkRemoteVerificationMessagesCleaned(ctx, account.ID, []string{"imap:1"}); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.RemoteVerificationMessageIDs(ctx, account.ID); err != nil || len(ids) != 0 {
		t.Fatalf("cleaned ids=%v err=%v", ids, err)
	}
	if err = s.SetSetting(ctx, "message_retention_days", "30"); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.CleanupMessages(ctx); err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if settings, err := s.AllSettings(ctx); err != nil || settings["message_retention_days"] != "30" {
		t.Fatalf("settings=%v err=%v", settings, err)
	}

	client, _, err := s.CreateAPIClient(ctx, "lease-client", []string{"leases:read", "leases:write", "codes:read"})
	if err != nil {
		t.Fatal(err)
	}
	lease, _, token, _, err := s.CreateLease(ctx, client, LeaseRequest{Project: "test", TTLSeconds: 60, IdempotencyKey: "lease-lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.LeaseByCredentials(ctx, client.ID, lease.ID, token); err != nil {
		t.Fatal(err)
	}
	if renewed, err := s.RenewLease(ctx, client.ID, lease.ID, token, 120); err != nil || !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	if _, err := s.FindCode(ctx, lease, now.Add(-time.Hour), "OpenAI", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("新租约不应读取旧验证码: %v", err)
	}
	if _, err := s.SaveMessages(ctx, []MessageInput{{MailboxID: lease.MailboxID, RemoteID: "imap:active-lease", Sender: "OpenAI", Code: "123456", ReceivedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	message, err := s.FindCode(ctx, lease, now.Add(-time.Hour), "OpenAI", false)
	if err != nil || message.Code != "123456" || message.ConsumedLeaseID != lease.ID {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	if err = s.ReleaseLease(ctx, client.ID, lease.ID, token, domain.LeaseReleased); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.LeaseByCredentials(ctx, client.ID, lease.ID, token); !errors.Is(err, ErrLeaseInactive) {
		t.Fatalf("released lease err=%v", err)
	}

	if err = s.QuickCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteMailboxSyncRejectsInvalidAndCrossAccountSnapshotsAtomically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.CreateAccount(ctx, "first@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAccount(ctx, "second@icloud.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := s.SaveCreatedRemoteMailbox(ctx, first.ID, RemoteMailboxInput{AnonymousID: "first-remote-id", Email: "shared@icloud.com", Active: true})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.SyncRemoteMailboxes(ctx, first.ID, []RemoteMailboxInput{
		{AnonymousID: "duplicate-one", Email: "duplicate@icloud.com", Active: true},
		{AnonymousID: "duplicate-two", Email: "DUPLICATE@icloud.com", Active: true},
	})
	if err == nil {
		t.Fatal("重复邮箱快照应被拒绝")
	}
	unchanged, err := s.Mailbox(ctx, mailbox.ID)
	if err != nil || !unchanged.RemotePresent || unchanged.Status != domain.MailboxAvailable || unchanged.RemoteAnonymousID != "first-remote-id" {
		t.Fatalf("无效快照不应修改既有邮箱: mailbox=%+v err=%v", unchanged, err)
	}

	_, err = s.SyncRemoteMailboxes(ctx, second.ID, []RemoteMailboxInput{{AnonymousID: "second-remote-id", Email: "shared@icloud.com", Active: true}})
	if err == nil || !strings.Contains(err.Error(), "其他账号") {
		t.Fatalf("跨账号邮箱冲突应被显式拒绝: %v", err)
	}
	unchanged, err = s.Mailbox(ctx, mailbox.ID)
	if err != nil || unchanged.AccountID != first.ID || unchanged.RemoteAnonymousID != "first-remote-id" || !unchanged.RemotePresent {
		t.Fatalf("跨账号冲突不应覆盖原记录: mailbox=%+v err=%v", unchanged, err)
	}
	secondRows, _, err := s.ListMailboxes(ctx, "", "", second.ID, "", 20)
	if err != nil || len(secondRows) != 0 {
		t.Fatalf("冲突账号不应写入邮箱: rows=%+v err=%v", secondRows, err)
	}
}
