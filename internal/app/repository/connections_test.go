package repository

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
)

func legacyV4Schema() string {
	s := strings.Replace(initialSchema, "('schema_version','5')", "('schema_version','4')", 1)
	s = strings.Replace(s, "allocation_mode TEXT NOT NULL DEFAULT 'shared' CHECK(allocation_mode IN ('shared','dedicated')), purpose TEXT NOT NULL DEFAULT '',\n  ", "", 1)
	s = s[:strings.Index(s, "CREATE TABLE imap_sync_states(")]
	return s
}
func legacyV3Schema() string {
	s := strings.Replace(legacyV4Schema(), "('schema_version','4')", "('schema_version','3')", 1)
	return strings.Replace(s, "  next_check_at TEXT, consecutive_failures INTEGER NOT NULL DEFAULT 0,\n", "", 1)
}

func TestConnectionMaintenancePersistsAndDoesNotOverwriteLogin(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "test@example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.UpsertConnection(ctx, a.ID, domain.ConnectionAppleAccount, domain.ConnectionHealthy, "appleid.apple.com", `{"token":"old"}`, "")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.SecretJSON = `{"token":"rotated"}`
	after.NextCheckAt = time.Now().UTC().Add(time.Minute)
	after.ConsecutiveFailures = 2
	if ok, err := s.SaveConnectionCheck(ctx, before, after); err != nil || !ok {
		t.Fatal("维护结果未保存", err)
	}
	current, err := s.ConnectionSecret(ctx, a.ID, before.Kind)
	if err != nil || current.SecretJSON != after.SecretJSON || current.ConsecutiveFailures != 2 || !current.NextCheckAt.Equal(after.NextCheckAt) {
		t.Fatal("会话与调度状态未一同保存")
	}
	if _, err = s.UpsertConnection(ctx, a.ID, before.Kind, domain.ConnectionChecking, "appleid.apple.com", `{"token":"new-login"}`, ""); err != nil {
		t.Fatal(err)
	}
	after.Status = domain.ConnectionExpired
	if ok, err := s.SaveConnectionCheck(ctx, current, after); err != nil || ok {
		t.Fatal("旧任务覆盖了新登录", err)
	}
	latest, _ := s.ConnectionSecret(ctx, a.ID, before.Kind)
	if latest.Status != domain.ConnectionChecking || latest.SecretJSON != `{"token":"new-login"}` || latest.ConsecutiveFailures != 0 || !latest.NextCheckAt.IsZero() {
		t.Fatal("新登录没有重置维护状态")
	}
}

func TestIMAPWatchCannotOverwriteUpdatedConnection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "watch@example.invalid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const oldSecret, newSecret = `{"email":"old@example.invalid"}`, `{"email":"new@example.invalid"}`
	for _, secret := range []string{oldSecret, newSecret} {
		if _, err := s.UpsertConnection(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionChecking, "imap.mail.me.com", secret, "等待检测"); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{domain.ConnectionHealthy, domain.ConnectionFailed} {
		if saved, err := s.SetIMAPWatchStatus(ctx, a.ID, oldSecret, status, "旧监听器"); err != nil || saved {
			t.Fatal("旧监听器覆盖了新凭据状态", err)
		}
	}
	current, _ := s.ConnectionSecret(ctx, a.ID, domain.ConnectionIMAP)
	if current.SecretJSON != newSecret || current.Status != domain.ConnectionChecking {
		t.Fatal("新连接状态发生变化")
	}
	if saved, err := s.SetIMAPWatchStatus(ctx, a.ID, newSecret, domain.ConnectionHealthy, "新监听器"); err != nil || !saved {
		t.Fatal("当前监听器未能更新状态", err)
	}
}

func TestConnectionMaintenanceScheduling(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _ := s.CreateAccount(ctx, "test@example.com", "", "")
	for _, kind := range []string{domain.ConnectionAppleAccount, domain.ConnectionICloudWeb, domain.ConnectionIMAP} {
		if _, err := s.UpsertConnection(ctx, a.ID, kind, domain.ConnectionHealthy, "", `{}`, ""); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.EnqueueConnectionMaintenance(ctx)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("仅应调度两个网页连接：jobs=%d err=%v", len(jobs), err)
	}
	if again, err := s.EnqueueConnectionMaintenance(ctx); err != nil || len(again) != 0 {
		t.Fatal("维护任务重复入队", err)
	}
	if _, created, err := s.EnsureConnectionCheckJob(ctx, a.ID, domain.ConnectionAppleAccount, false); err != nil || created {
		t.Fatal("手动检测未复用已有任务", err)
	}
	for _, job := range jobs {
		if err := s.CompleteJob(ctx, job.ID, domain.JobSucceeded, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{domain.ConnectionAppleAccount, domain.ConnectionICloudWeb} {
		before, _ := s.ConnectionSecret(ctx, a.ID, kind)
		after := before
		after.NextCheckAt = time.Now().Add(time.Hour)
		if kind == domain.ConnectionICloudWeb {
			after.Status = domain.ConnectionExpired
			after.NextCheckAt = time.Time{}
		}
		if ok, err := s.SaveConnectionCheck(ctx, before, after); err != nil || !ok {
			t.Fatal(err)
		}
	}
	if again, err := s.EnqueueConnectionMaintenance(ctx); err != nil || len(again) != 0 {
		t.Fatal("未来任务或待登录连接不应自动请求", err)
	}
}

func TestV3MigrationPreservesConnectionsAndMaintenanceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, legacyV3Schema()); err != nil {
		t.Fatal(err)
	}
	now := nowText()
	if _, err = db.ExecContext(ctx, `INSERT INTO apple_accounts(id,apple_id,created_at,updated_at) VALUES('legacy','legacy@example.com',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO account_connections(id,account_id,kind,status,secret_json,created_at,updated_at) VALUES('connection','legacy','icloud_web','failed','{"note":"old-credentials"}',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(time.Hour)
	for i := 0; i < 2; i++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ConnectionSecret(ctx, "legacy", domain.ConnectionICloudWeb)
		if err != nil {
			t.Fatal(err)
		}
		if got.SecretJSON != `{"note":"old-credentials"}` {
			t.Fatal("升级或重启丢失历史凭据")
		}
		if i == 0 {
			if !got.NextCheckAt.IsZero() || got.ConsecutiveFailures != 0 {
				t.Fatal("旧连接维护字段初值错误")
			}
			after := got
			after.NextCheckAt = next
			after.ConsecutiveFailures = 3
			if ok, err := s.SaveConnectionCheck(ctx, got, after); err != nil || !ok {
				t.Fatal("维护结果保存失败", err)
			}
		} else if !got.NextCheckAt.Equal(next) || got.ConsecutiveFailures != 3 {
			t.Fatal("重启后维护进度丢失")
		}
		if err := s.IntegrityCheck(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeletedConnectionRejectsStaleMaintenance(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "test@example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.UpsertConnection(ctx, a.ID, domain.ConnectionICloudWeb, domain.ConnectionHealthy, "", `{}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnection(ctx, a.ID, before.Kind); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertConnection(ctx, a.ID, before.Kind, domain.ConnectionChecking, "", `{"note":"new"}`, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SaveConnectionCheck(ctx, before, before); err != nil || ok {
		t.Fatal("已删除连接的旧任务覆盖了重建的连接", err)
	}
}

func TestOptionalConnectionDoesNotAffectBasicHealth(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "basic@example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{domain.ConnectionICloudWeb, domain.ConnectionIMAP, domain.ConnectionAppleAccount} {
		status := domain.ConnectionHealthy
		if kind == domain.ConnectionAppleAccount {
			status = domain.ConnectionExpired
		}
		if _, err := s.UpsertConnection(ctx, a.ID, kind, status, "", `{"note":"preserved"}`, ""); err != nil {
			t.Fatal(err)
		}
	}
	// 模拟升级前遗留的账号总状态，读取时应按基础连接重新计算。
	if _, err := s.DB().Exec(`UPDATE apple_accounts SET status='failed' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	account, err := s.Account(ctx, a.ID)
	if err != nil || account.Status != domain.ConnectionHealthy {
		t.Fatal("备用通道影响了基础连接状态", err)
	}
	dashboard, err := s.Dashboard(ctx)
	if err != nil || dashboard["account_issues"] != 0 {
		t.Fatal("备用通道计入基础连接故障", dashboard, err)
	}
	before, err := s.ConnectionSecret(ctx, a.ID, domain.ConnectionAppleAccount)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := s.SetOptionalConnectionEnabled(ctx, a.ID, false)
	if err != nil || disabled.Status != domain.ConnectionDisabled || disabled.SecretJSON != before.SecretJSON || !disabled.NextCheckAt.IsZero() {
		t.Fatal("停用没有保留凭据或停止调度", err)
	}
	if saved, err := s.SaveConnectionCheck(ctx, before, before); err != nil || saved {
		t.Fatal("在途检测覆盖了停用状态", err)
	}
	enabled, err := s.SetOptionalConnectionEnabled(ctx, a.ID, true)
	if err != nil || enabled.Status != domain.ConnectionChecking || enabled.SecretJSON != before.SecretJSON {
		t.Fatal("启用未保留凭据并等待检测", err)
	}
	if _, err := s.SetOptionalConnectionEnabled(ctx, a.ID, true); err != nil {
		t.Fatal("重复启用应可安全执行", err)
	}
	if err := s.SetConnectionStatus(ctx, a.ID, domain.ConnectionIMAP, domain.ConnectionFailed, "测试故障"); err != nil {
		t.Fatal(err)
	}
	account, err = s.Account(ctx, a.ID)
	if err != nil || account.Status != domain.ConnectionFailed {
		t.Fatal("基础收件故障应反映在账号状态中", err)
	}
}
