package repository

import (
	"context"
	"testing"
	"time"

	"small-apple-mail/internal/app/domain"
)

func TestListsWorkWithOneAvailableConnection(t *testing.T) {
	for _, kind := range []string{"accounts", "clients"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			account, err := s.CreateAccount(ctx, "owner@icloud.com", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.UpsertConnection(ctx, account.ID, domain.ConnectionIMAP, domain.ConnectionHealthy, "imap.mail.me.com", "{}", ""); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.CreateAPIClient(ctx, "client", []string{"codes:read"}); err != nil {
				t.Fatal(err)
			}
			s.db.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			if kind == "accounts" {
				rows, err := s.ListAccounts(ctx)
				if err != nil || len(rows) != 1 || len(rows[0].Connections) != 1 {
					t.Fatalf("账号列表查询占用连接后无法继续: rows=%v err=%v", rows, err)
				}
			} else {
				rows, err := s.ListAPIClients(ctx)
				if err != nil || len(rows) != 1 || len(rows[0].Scopes) != 1 {
					t.Fatalf("客户端列表查询占用连接后无法继续: rows=%v err=%v", rows, err)
				}
			}
		})
	}
}
