package repository

import (
	"context"
	"fmt"
	"slices"
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

func TestListAssociationsStayWithTheirOwner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	accountKinds := map[string][]string{}
	clientScopes := map[string][]string{}
	for i := 0; i < 3; i++ {
		account, err := s.CreateAccount(ctx, fmt.Sprintf("owner%d@icloud.com", i), "", "")
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, kind := range []string{domain.ConnectionICloudWeb, domain.ConnectionIMAP}[:i] {
			if _, err := s.UpsertConnection(ctx, account.ID, kind, domain.ConnectionHealthy, "", "{}", ""); err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, kind)
		}
		accountKinds[account.ID] = kinds
		scopes := []string{"codes:read", "leases:read"}[:i]
		client, _, err := s.CreateAPIClient(ctx, fmt.Sprintf("client%d", i), scopes)
		if err != nil {
			t.Fatal(err)
		}
		clientScopes[client.ID] = scopes
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil || len(accounts) != 3 {
		t.Fatalf("账号列表：len=%d err=%v", len(accounts), err)
	}
	for _, account := range accounts {
		var kinds []string
		for _, connection := range account.Connections {
			if connection.AccountID != account.ID {
				t.Fatalf("连接归属错误：%+v", connection)
			}
			kinds = append(kinds, connection.Kind)
		}
		if !slices.Equal(kinds, accountKinds[account.ID]) {
			t.Errorf("连接种类=%v，期望 %v", kinds, accountKinds[account.ID])
		}
	}
	clients, err := s.ListAPIClients(ctx)
	if err != nil || len(clients) != 3 {
		t.Fatalf("客户端列表：len=%d err=%v", len(clients), err)
	}
	for _, client := range clients {
		if !slices.Equal(client.Scopes, clientScopes[client.ID]) {
			t.Errorf("客户端权限=%v，期望 %v", client.Scopes, clientScopes[client.ID])
		}
	}
}
