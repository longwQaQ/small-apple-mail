package repository

import (
	"context"
	"path/filepath"
	"testing"
)

func seedDashboard(t testing.TB, s *Store) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(), `
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<20)
INSERT INTO apple_accounts(id,apple_id,created_at,updated_at)
SELECT 'acc_'||i,'owner'||i||'@icloud.com','2026-01-01','2026-01-01' FROM n;
INSERT INTO account_connections(id,account_id,kind,status,created_at,updated_at) VALUES
('con_1','acc_1','imap','healthy','2026-01-01','2026-01-01'),
('con_2','acc_2','imap','failed','2026-01-01','2026-01-01'),
('con_3','acc_1','icloud_web','healthy','2026-01-01','2026-01-01');
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000)
INSERT INTO mailboxes(id,account_id,email,status,allocation_mode,created_at,updated_at)
SELECT 'mbx_'||i,'acc_'||(i%20+1),'mail'||i||'@icloud.com',
CASE i%4 WHEN 0 THEN 'available' WHEN 1 THEN 'leased' WHEN 2 THEN 'disabled' ELSE 'remote_missing' END,
CASE WHEN i%8=0 THEN 'dedicated' ELSE 'shared' END,'2026-01-01','2026-01-01' FROM n;
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<10000)
INSERT INTO messages(id,mailbox_id,remote_id,received_at,synced_at)
SELECT 'msg_'||i,'mbx_'||(i%1000+1),'remote_'||i,
CASE WHEN i%2=0 THEN '2099-01-01' ELSE '2000-01-01' END,'2026-01-01' FROM n;
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<2000)
INSERT INTO jobs(id,type,status,created_at)
SELECT 'job_'||i,'database.integrity_check',
CASE i%4 WHEN 0 THEN 'queued' WHEN 1 THEN 'running' WHEN 2 THEN 'failed' ELSE 'succeeded' END,'2026-01-01' FROM n;
INSERT INTO api_clients(id,name,key_prefix,key_hash,created_at,updated_at)
VALUES('client','测试客户端','test_prefix','test_hash','2026-01-01','2026-01-01');
INSERT INTO leases(id,mailbox_id,api_client_id,token_hash,idempotency_key,request_hash,status,created_at,updated_at,expires_at) VALUES
('lease_1','mbx_1','client','hash_1','key_1','request_1','active','2026-01-01','2026-01-01','2099-01-01'),
('lease_2','mbx_2','client','hash_2','key_2','request_2','active','2026-01-01','2026-01-01','2000-01-01');
INSERT INTO schedules(id,type,name,interval_seconds,next_run_at,enabled,created_at,updated_at) VALUES
('schedule_1','database.integrity_check','下一次计划',3600,'2099-01-01',1,'2026-01-01','2026-01-01'),
('schedule_2','database.integrity_check','停用计划',3600,'2000-01-01',0,'2026-01-01','2026-01-01');`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDashboardSummary(t *testing.T) {
	for _, populated := range []bool{false, true} {
		name := "空数据库"
		if populated {
			name = "包含业务数据"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			if populated {
				seedDashboard(t, s)
			}
			got, err := s.Dashboard(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int{
				"accounts": 20, "account_issues": 1, "accounts_without_create_connection": 19, "accounts_without_imap": 19,
				"mailboxes": 1000, "available_mailboxes": 125, "leased_mailboxes": 250, "disabled_mailboxes": 250, "remote_missing_mailboxes": 250,
				"active_leases": 1, "messages_24h": 5000, "running_jobs": 1000, "failed_jobs": 500,
				"healthy_imap_connections": 1, "configured_imap_connections": 2,
			}
			for key, n := range want {
				if !populated {
					n = 0
				}
				if got[key] != n {
					t.Errorf("%s=%v，期望 %d", key, got[key], n)
				}
			}
			if populated && got["next_schedule_name"] != "下一次计划" {
				t.Errorf("下次计划=%v", got["next_schedule_name"])
			}
			if !populated && (got["next_schedule_name"] != "" || got["next_schedule_at"] != "") {
				t.Errorf("空数据库不应有下次计划：%v", got)
			}
		})
	}
}

func BenchmarkDashboard(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "dashboard.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	seedDashboard(b, s)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Dashboard(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdminLists(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "lists.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	seedDashboard(b, s)
	_, err = s.db.ExecContext(ctx, `
WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100)
INSERT INTO api_clients(id,name,key_prefix,key_hash,created_at,updated_at)
SELECT 'client_'||i,'测试客户端'||i,'prefix_'||i,'hash_'||i,'2026-01-01','2026-01-01' FROM n;
INSERT INTO api_client_scopes(api_client_id,scope) SELECT id,'codes:read' FROM api_clients;
INSERT INTO api_client_scopes(api_client_id,scope) SELECT id,'leases:read' FROM api_clients;`)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("账号列表", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.ListAccounts(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("客户端列表", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.ListAPIClients(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
