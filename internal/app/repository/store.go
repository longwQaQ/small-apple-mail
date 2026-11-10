package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"small-apple-mail/internal/app/domain"
	"small-apple-mail/internal/app/security"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db      *sql.DB
	path    string
	claimMu sync.Mutex
	jobMu   sync.Mutex
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("数据库路径不能为空")
	}
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录: %w", err)
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("设置数据库目录权限: %w", err)
		}
	}
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 SQLite: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置数据库文件权限: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		if _, err := tx.ExecContext(ctx, initialSchema); err != nil {
			return fmt.Errorf("初始化数据库结构: %w", err)
		}
	} else {
		var appID, version string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM application_meta WHERE key='application_id'").Scan(&appID); err != nil {
			return fmt.Errorf("数据库不是 small-apple-mail 首次部署结构，请使用全新数据库文件: %w", err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT value FROM application_meta WHERE key='schema_version'").Scan(&version); err != nil {
			return fmt.Errorf("数据库缺少结构版本: %w", err)
		}
		if appID != applicationID {
			return fmt.Errorf("不支持的数据库结构: application_id=%q schema_version=%q", appID, version)
		}
		if version == "1" {
			if _, err := tx.ExecContext(ctx, migrateV1ToV2); err != nil {
				return fmt.Errorf("升级数据库结构到版本 2: %w", err)
			}
			version = "2"
		}
		if version == "2" {
			if _, err := tx.ExecContext(ctx, migrateV2ToV3); err != nil {
				return fmt.Errorf("升级数据库结构到版本 3: %w", err)
			}
			version = "3"
		}
		if version == "3" {
			if _, err := tx.ExecContext(ctx, migrateV3ToV4); err != nil {
				return fmt.Errorf("升级数据库结构到版本 4: %w", err)
			}
			version = "4"
		}
		if version == "4" {
			if _, err := tx.ExecContext(ctx, migrateV4ToV5); err != nil {
				return fmt.Errorf("升级数据库结构到版本 5: %w", err)
			}
			version = "5"
		}
		if version != fmt.Sprint(schemaVersion) {
			return fmt.Errorf("不支持的数据库结构: application_id=%q schema_version=%q", appID, version)
		}
	}
	defaults := map[string]string{
		"message_retention_days": "30",
	}
	for key, value := range defaults {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO settings(key,value,updated_at) VALUES(?,?,?)", key, value, nowText()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key='debug_body_retention_hours'"); err != nil {
		return err
	}
	return tx.Commit()
}

func nowText() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func timeText(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func parseTime(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v.String)
	return t
}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite 完整性检查失败: %s", result)
	}
	var violations int
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		violations++
	}
	if violations > 0 {
		return fmt.Errorf("发现 %d 个外键错误", violations)
	}
	return rows.Err()
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM application_meta WHERE key='schema_version'").Scan(&version); err != nil {
		return 0, err
	}
	if version != fmt.Sprint(schemaVersion) {
		return 0, fmt.Errorf("不支持的数据库结构版本 %q", version)
	}
	return schemaVersion, nil
}

func (s *Store) Ready(ctx context.Context) error {
	var one int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	_, err := s.SchemaVersion(ctx)
	return err
}

func (s *Store) QuickCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite 快速检查失败: %s", result)
	}
	return nil
}

func (s *Store) BootstrapAdmin(ctx context.Context, username, password string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 64 {
		return fmt.Errorf("首次启动必须提供 3-64 位管理员用户名")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	id, err := security.ID("adm")
	if err != nil {
		return err
	}
	now := nowText()
	_, err = s.db.ExecContext(ctx, "INSERT INTO admins(id,username,password_hash,password_changed_at,created_at) VALUES(?,?,?,?,?)", id, username, hash, now, now)
	return err
}

func (s *Store) AdminByUsername(ctx context.Context, username string) (domain.Admin, error) {
	var a domain.Admin
	var changed, last, created sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT id,username,password_hash,password_changed_at,last_login_at,created_at FROM admins WHERE username=? COLLATE NOCASE", strings.TrimSpace(username)).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &changed, &last, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.PasswordChangedAt, a.LastLoginAt, a.CreatedAt = parseTime(changed), parseTime(last), parseTime(created)
	return a, err
}

func (s *Store) Admin(ctx context.Context) (domain.Admin, error) {
	var username string
	if err := s.db.QueryRowContext(ctx, "SELECT username FROM admins LIMIT 1").Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Admin{}, ErrNotFound
		}
		return domain.Admin{}, err
	}
	return s.AdminByUsername(ctx, username)
}

func (s *Store) MarkAdminLogin(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE admins SET last_login_at=? WHERE id=?", nowText(), id)
	return err
}

func (s *Store) ChangePassword(ctx context.Context, adminID, hash string, keepSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE admins SET password_hash=?,password_changed_at=? WHERE id=?", hash, nowText(), adminID); err != nil {
		return err
	}
	if keepSessionID == "" {
		_, err = tx.ExecContext(ctx, "DELETE FROM web_sessions")
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM web_sessions WHERE id<>?", keepSessionID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateSession(ctx context.Context, idle, absolute time.Duration) (domain.Session, string, string, error) {
	id, _ := security.ID("ses")
	token, _ := security.RandomToken("ses_", 32)
	now := time.Now().UTC()
	tokenHash := security.HashToken(token)
	csrf := security.CSRFToken(tokenHash)
	session := domain.Session{ID: id, TokenHash: tokenHash, CSRFHash: security.HashToken(csrf), CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(idle), AbsoluteExpiresAt: now.Add(absolute)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions(id,token_hash,csrf_hash,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES(?,?,?,?,?,?,?)`,
		session.ID, session.TokenHash, session.CSRFHash, timeText(now), timeText(now), timeText(session.IdleExpiresAt), timeText(session.AbsoluteExpiresAt))
	return session, token, csrf, err
}

func (s *Store) SessionByToken(ctx context.Context, token string, idle time.Duration) (domain.Session, error) {
	var v domain.Session
	var created, last, idleExp, absolute sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,token_hash,csrf_hash,created_at,last_seen_at,idle_expires_at,absolute_expires_at FROM web_sessions WHERE token_hash=?`, security.HashToken(token)).
		Scan(&v.ID, &v.TokenHash, &v.CSRFHash, &created, &last, &idleExp, &absolute)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.CreatedAt, v.LastSeenAt, v.IdleExpiresAt, v.AbsoluteExpiresAt = parseTime(created), parseTime(last), parseTime(idleExp), parseTime(absolute)
	now := time.Now().UTC()
	if !v.IdleExpiresAt.After(now) || !v.AbsoluteExpiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE id=?", v.ID)
		return domain.Session{}, ErrNotFound
	}
	newIdle := now.Add(idle)
	if newIdle.After(v.AbsoluteExpiresAt) {
		newIdle = v.AbsoluteExpiresAt
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE web_sessions SET last_seen_at=?,idle_expires_at=? WHERE id=?", timeText(now), timeText(newIdle), v.ID)
	v.LastSeenAt, v.IdleExpiresAt = now, newIdle
	return v, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE id=?", id)
	return err
}

func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,apple_id,display_name,note,(`+accountConnectionStatusSQL+`),created_at,updated_at FROM apple_accounts ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Account
	for rows.Next() {
		var a domain.Account
		var c, u sql.NullString
		if err := rows.Scan(&a.ID, &a.AppleID, &a.DisplayName, &a.Note, &a.Status, &c, &u); err != nil {
			return nil, err
		}
		a.CreatedAt, a.UpdatedAt = parseTime(c), parseTime(u)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	connections, err := s.ListConnections(ctx, "")
	if err != nil {
		return nil, err
	}
	byAccount := make(map[string][]domain.Connection, len(out))
	for _, connection := range connections {
		byAccount[connection.AccountID] = append(byAccount[connection.AccountID], connection)
	}
	for i := range out {
		out[i].Connections = byAccount[out[i].ID]
	}
	return out, nil
}

func (s *Store) CreateAccount(ctx context.Context, appleID, name, note string) (domain.Account, error) {
	id, _ := security.ID("acc")
	now := time.Now().UTC()
	a := domain.Account{ID: id, AppleID: strings.ToLower(strings.TrimSpace(appleID)), DisplayName: strings.TrimSpace(name), Note: strings.TrimSpace(note), Status: domain.ConnectionNotConfigured, CreatedAt: now, UpdatedAt: now}
	if a.AppleID == "" {
		return a, fmt.Errorf("Apple ID 不能为空")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO apple_accounts(id,apple_id,display_name,note,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, a.ID, a.AppleID, a.DisplayName, a.Note, a.Status, timeText(now), timeText(now))
	return a, err
}

func (s *Store) ListMailboxes(ctx context.Context, query, status, accountID, cursor string, limit int) ([]domain.Mailbox, string, error) {
	return s.FilterMailboxes(ctx, MailboxFilter{Query: query, Status: status, AccountID: accountID, Cursor: cursor, Limit: limit})
}

func (s *Store) FilterMailboxes(ctx context.Context, filter MailboxFilter) ([]domain.Mailbox, string, error) {
	query, status, accountID, cursor, limit := filter.Query, filter.Status, filter.AccountID, filter.Cursor, filter.Limit
	if limit < 1 || limit > 200 {
		limit = 50
	}
	args := []any{}
	where := []string{"1=1"}
	if query != "" {
		where = append(where, "(email LIKE ? OR label LIKE ? OR note LIKE ? OR purpose LIKE ? OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(tags_json) THEN tags_json ELSE '[]' END) WHERE value LIKE ?))")
		q := "%" + query + "%"
		args = append(args, q, q, q, q, q)
	}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if accountID != "" {
		where = append(where, "account_id=?")
		args = append(args, accountID)
	}
	if filter.Mode != "" {
		where = append(where, "allocation_mode=?")
		args = append(args, filter.Mode)
	}
	if filter.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(tags_json) THEN tags_json ELSE '[]' END) WHERE value=? COLLATE NOCASE)")
		args = append(args, filter.Tag)
	}
	if filter.WithResult {
		where = append(where, "EXISTS (SELECT 1 FROM messages WHERE mailbox_id=mailboxes.id AND (code<>'' OR login_url<>''))")
	}
	if filter.Unused {
		where = append(where, "last_leased_at IS NULL AND allocation_mode='shared'")
	}
	if cursor != "" {
		where = append(where, "id<?")
		args = append(args, cursor)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT id,account_id,email,COALESCE(remote_anonymous_id,''),label,note,allocation_mode,purpose,tags_json,status,remote_status,local_enabled,remote_present,remote_enabled,last_leased_at,last_synced_at,
		COALESCE((SELECT msg.code FROM messages msg WHERE msg.mailbox_id=mailboxes.id AND msg.code<>'' ORDER BY `+messageTimeSQL+` DESC,msg.id DESC LIMIT 1),''),
		(SELECT msg.received_at FROM messages msg WHERE msg.mailbox_id=mailboxes.id AND msg.code<>'' ORDER BY `+messageTimeSQL+` DESC,msg.id DESC LIMIT 1),
		COALESCE((SELECT msg.login_url FROM messages msg WHERE msg.mailbox_id=mailboxes.id AND msg.login_url<>'' ORDER BY `+messageTimeSQL+` DESC,msg.id DESC LIMIT 1),''),
		(SELECT msg.received_at FROM messages msg WHERE msg.mailbox_id=mailboxes.id AND msg.login_url<>'' ORDER BY `+messageTimeSQL+` DESC,msg.id DESC LIMIT 1),
		created_at,updated_at FROM mailboxes WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []domain.Mailbox
	for rows.Next() {
		var m domain.Mailbox
		var tags string
		var local, present, enabled int
		var leased, synced, code, loginURLAt, created, updated sql.NullString
		if err := rows.Scan(&m.ID, &m.AccountID, &m.Email, &m.RemoteAnonymousID, &m.Label, &m.Note, &m.AllocationMode, &m.Purpose, &tags, &m.Status, &m.RemoteStatus, &local, &present, &enabled, &leased, &synced, &m.LastCode, &code, &m.LastLoginURL, &loginURLAt, &created, &updated); err != nil {
			return nil, "", err
		}
		_ = json.Unmarshal([]byte(tags), &m.Tags)
		m.LocalEnabled, m.RemotePresent, m.RemoteEnabled = local != 0, present != 0, enabled != 0
		m.LastLeasedAt, m.LastSyncedAt, m.LastCodeAt, m.CreatedAt, m.UpdatedAt = parseTime(leased), parseTime(synced), parseTime(code), parseTime(created), parseTime(updated)
		m.LastLoginURLAt = parseTime(loginURLAt)
		out = append(out, m)
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].ID
		out = out[:limit]
	}
	return out, next, rows.Err()
}

// AllMailboxesByAccount 返回账号下的全部邮箱。面向管理后台的 ListMailboxes
// 保持分页，而消息匹配、远端同步预览等后台业务不能丢弃分页后的邮箱。
func (s *Store) AllMailboxesByAccount(ctx context.Context, accountID string) ([]domain.Mailbox, error) {
	var out []domain.Mailbox
	cursor := ""
	for {
		rows, next, err := s.ListMailboxes(ctx, "", "", accountID, cursor, 200)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

func (s *Store) CreateMailbox(ctx context.Context, accountID, email, label, note string, tags []string, options ...MailboxPatch) (domain.Mailbox, error) {
	id, _ := security.ID("mbx")
	now := time.Now().UTC()
	mode, purpose := "shared", ""
	if len(options) > 0 {
		if options[0].AllocationMode != nil && *options[0].AllocationMode != "" {
			mode = *options[0].AllocationMode
		}
		if options[0].Purpose != nil {
			purpose = strings.TrimSpace(*options[0].Purpose)
		}
	}
	if mode != "shared" && mode != "dedicated" {
		return domain.Mailbox{}, fmt.Errorf("无效邮箱用途类型")
	}
	if len([]rune(purpose)) > 120 {
		return domain.Mailbox{}, fmt.Errorf("用途说明最多 120 个字符")
	}
	tags, err := normalizeTags(tags)
	if err != nil {
		return domain.Mailbox{}, err
	}
	tagJSON, _ := json.Marshal(tags)
	m := domain.Mailbox{ID: id, AccountID: accountID, Email: strings.ToLower(strings.TrimSpace(email)), Label: label, Note: note, Tags: tags, AllocationMode: mode, Purpose: purpose, Status: domain.MailboxAvailable, RemoteStatus: "active", LocalEnabled: true, RemotePresent: true, RemoteEnabled: true, CreatedAt: now, UpdatedAt: now}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mailboxes(id,account_id,email,label,note,allocation_mode,purpose,tags_json,status,remote_status,local_enabled,remote_present,remote_enabled,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,1,1,1,?,?)`, id, accountID, m.Email, label, note, mode, purpose, string(tagJSON), m.Status, m.RemoteStatus, timeText(now), timeText(now))
	return m, err
}

func (s *Store) CreateAPIClient(ctx context.Context, name string, scopes []string) (domain.APIClient, string, error) {
	for _, scope := range scopes {
		if !domain.ValidScopes[scope] {
			return domain.APIClient{}, "", fmt.Errorf("无效权限域: %s", scope)
		}
	}
	id, _ := security.ID("cli")
	key, _ := security.RandomToken("sam_live_", 32)
	prefix := key
	if len(prefix) > 18 {
		prefix = prefix[:18]
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.APIClient{}, "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO api_clients(id,name,key_prefix,key_hash,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, strings.TrimSpace(name), prefix, security.HashToken(key), "active", timeText(now), timeText(now))
	if err != nil {
		return domain.APIClient{}, "", err
	}
	for _, scope := range scopes {
		if _, err = tx.ExecContext(ctx, "INSERT INTO api_client_scopes(api_client_id,scope) VALUES(?,?)", id, scope); err != nil {
			return domain.APIClient{}, "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.APIClient{}, "", err
	}
	return domain.APIClient{ID: id, Name: name, KeyPrefix: prefix, Status: "active", Scopes: scopes, CreatedAt: now, UpdatedAt: now}, key, nil
}

func (s *Store) ListAPIClients(ctx context.Context) ([]domain.APIClient, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,key_prefix,key_hash,status,last_used_at,created_at,updated_at FROM api_clients ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.APIClient
	for rows.Next() {
		var c domain.APIClient
		var last, created, updated sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.KeyPrefix, &c.KeyHash, &c.Status, &last, &created, &updated); err != nil {
			return nil, err
		}
		c.LastUsedAt, c.CreatedAt, c.UpdatedAt = parseTime(last), parseTime(created), parseTime(updated)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	scopeRows, err := s.db.QueryContext(ctx, "SELECT api_client_id,scope FROM api_client_scopes ORDER BY api_client_id,scope")
	if err != nil {
		return nil, err
	}
	defer scopeRows.Close()
	byClient := make(map[string][]string, len(out))
	for scopeRows.Next() {
		var id, scope string
		if err := scopeRows.Scan(&id, &scope); err != nil {
			return nil, err
		}
		byClient[id] = append(byClient[id], scope)
	}
	if err := scopeRows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Scopes = byClient[out[i].ID]
	}
	return out, nil
}

func (s *Store) apiClientScopes(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT scope FROM api_client_scopes WHERE api_client_id=? ORDER BY scope", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

func (s *Store) APIClientByKey(ctx context.Context, key string) (domain.APIClient, error) {
	var c domain.APIClient
	var last, created, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,name,key_prefix,key_hash,status,last_used_at,created_at,updated_at FROM api_clients WHERE key_hash=?`, security.HashToken(key)).Scan(&c.ID, &c.Name, &c.KeyPrefix, &c.KeyHash, &c.Status, &last, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.LastUsedAt, c.CreatedAt, c.UpdatedAt = parseTime(last), parseTime(created), parseTime(updated)
	c.Scopes, err = s.apiClientScopes(ctx, c.ID)
	if err != nil {
		return c, err
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE api_clients SET last_used_at=? WHERE id=?", nowText(), c.ID)
	return c, nil
}

func HasScope(c domain.APIClient, scope string) bool {
	for _, v := range c.Scopes {
		if v == scope {
			return true
		}
	}
	return false
}

func (s *Store) CreateJob(ctx context.Context, typ, payload string, priority int) (domain.Job, error) {
	id, _ := security.ID("job")
	now := time.Now().UTC()
	j := domain.Job{ID: id, Type: typ, Status: domain.JobQueued, Priority: priority, Payload: payload, CreatedAt: now}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(id,type,status,priority,payload_json,created_at) VALUES(?,?,?,?,?,?)`, id, typ, j.Status, priority, payload, timeText(now))
	return j, err
}

// EnsureMessageSyncJob 保证同一账号最多存在一个排队、运行或取消中的消息同步任务。
func (s *Store) EnsureMessageSyncJob(ctx context.Context, accountID string, priority int) (domain.Job, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return domain.Job{}, false, fmt.Errorf("消息同步账号不能为空")
	}
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type='mailbox.message_sync' AND status IN ('queued','running','cancelling') AND json_valid(payload_json) AND json_extract(payload_json,'$.account_id')=? ORDER BY created_at LIMIT 1`, accountID).Scan(&id)
	if err == nil {
		job, loadErr := s.Job(ctx, id)
		return job, false, loadErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, false, err
	}
	payload, err := json.Marshal(map[string]string{"account_id": accountID})
	if err != nil {
		return domain.Job{}, false, err
	}
	job, err := s.CreateJob(ctx, "mailbox.message_sync", string(payload), priority)
	return job, err == nil, err
}

func (s *Store) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,status,priority,payload_json,progress,attempts,error_code,error_message,created_at,started_at,finished_at FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		var j domain.Job
		var created, started, finished sql.NullString
		if err := rows.Scan(&j.ID, &j.Type, &j.Status, &j.Priority, &j.Payload, &j.Progress, &j.Attempts, &j.ErrorCode, &j.Error, &created, &started, &finished); err != nil {
			return nil, err
		}
		j.CreatedAt, j.StartedAt, j.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) ListJobsPage(ctx context.Context, cursor string, after time.Time, limit int) ([]domain.Job, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	where := []string{"created_at>=?"}
	args := []any{timeText(after)}
	if cursor != "" {
		var cursorTime string
		if err := s.db.QueryRowContext(ctx, "SELECT created_at FROM jobs WHERE id=?", cursor).Scan(&cursorTime); errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		} else if err != nil {
			return nil, "", err
		}
		where = append(where, "(created_at<? OR (created_at=? AND id<?))")
		args = append(args, cursorTime, cursorTime, cursor)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT id,type,status,priority,payload_json,progress,attempts,error_code,error_message,created_at,started_at,finished_at FROM jobs WHERE `+strings.Join(where, " AND ")+` ORDER BY created_at DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		var job domain.Job
		var created, started, finished sql.NullString
		if err := rows.Scan(&job.ID, &job.Type, &job.Status, &job.Priority, &job.Payload, &job.Progress, &job.Attempts, &job.ErrorCode, &job.Error, &created, &started, &finished); err != nil {
			return nil, "", err
		}
		job.CreatedAt, job.StartedAt, job.FinishedAt = parseTime(created), parseTime(started), parseTime(finished)
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].ID
		out = out[:limit]
	}
	return out, next, nil
}

func (s *Store) DeleteJobsBefore(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cutoff := timeText(before)
	if _, err = tx.ExecContext(ctx, `DELETE FROM job_events WHERE job_id IN (SELECT id FROM jobs WHERE created_at<? AND status NOT IN ('queued','running','cancelling'))`, cutoff); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE created_at<? AND status NOT IN ('queued','running','cancelling')`, cutoff)
	if err != nil {
		return 0, err
	}
	removed, _ := result.RowsAffected()
	return removed, tx.Commit()
}

func (s *Store) DeleteAllJobs(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM jobs")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) CancelAllJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=CASE WHEN status='queued' THEN 'cancelled' ELSE 'cancelling' END,finished_at=CASE WHEN status='queued' THEN ? ELSE finished_at END WHERE status IN ('queued','running')`, nowText())
	return err
}

func (s *Store) ClaimJob(ctx context.Context) (domain.Job, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, err
	}
	defer tx.Rollback()
	var id string
	// 收件同步和远端清理共用 UID 命名空间，避免读取清理目标后代次被同步任务改写。
	err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE status='queued' AND (type NOT IN ('mailbox.message_sync','mailbox.remote_clean') OR NOT EXISTS (SELECT 1 FROM jobs running WHERE running.type IN ('mailbox.message_sync','mailbox.remote_clean') AND running.status IN ('running','cancelling'))) ORDER BY priority DESC,created_at LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Job{}, ErrNotFound
	}
	if err != nil {
		return domain.Job{}, err
	}
	now := nowText()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running',started_at=?,attempts=attempts+1 WHERE id=? AND status='queued'`, now, id)
	if err != nil {
		return domain.Job{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return domain.Job{}, ErrNotFound
	}
	if err = tx.Commit(); err != nil {
		return domain.Job{}, err
	}
	return s.Job(ctx, id)
}

func (s *Store) CompleteJob(ctx context.Context, id, status, errorCode, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var scheduleID sql.NullString
	if err = tx.QueryRowContext(ctx, "SELECT schedule_id FROM jobs WHERE id=?", id).Scan(&scheduleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?,progress=CASE WHEN ?='succeeded' THEN 100 ELSE progress END,error_code=?,error_message=?,finished_at=? WHERE id=?`, status, status, errorCode, message, now, id); err != nil {
		return err
	}
	if scheduleID.Valid {
		switch status {
		case domain.JobSucceeded:
			_, err = tx.ExecContext(ctx, `UPDATE schedules SET consecutive_failures=0,paused_reason='',updated_at=? WHERE id=?`, now, scheduleID.String)
		case domain.JobFailed, domain.JobPartiallySucceeded:
			_, err = tx.ExecContext(ctx, `UPDATE schedules SET consecutive_failures=consecutive_failures+1,paused_reason=CASE WHEN consecutive_failures+1>=3 THEN '连续失败 3 次，已自动暂停' ELSE paused_reason END,updated_at=? WHERE id=?`, now, scheduleID.String)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AddJobEvent(ctx context.Context, jobID, stage, level, message, detail string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO job_events(job_id,stage,level,message,detail_json,created_at) VALUES(?,?,?,?,?,?)`, jobID, stage, level, message, detail, nowText())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) InterruptRunningJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='interrupted',finished_at=?,error_code='service_restarted',error_message='服务重启导致任务中断' WHERE status IN ('running','cancelling')`, nowText())
	return err
}

func (s *Store) CreateActivity(ctx context.Context, e domain.Activity) error {
	if e.Source == "" {
		e.Source = "system"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO activities(source,type,result,entity_type,entity_id,request_id,remote_ip,api_client_id,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, e.Source, e.Type, e.Result, e.EntityType, e.EntityID, e.RequestID, e.RemoteIP, e.APIClientID, e.Detail, nowText())
	return err
}

func (s *Store) Dashboard(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	queries := map[string]string{"accounts": "SELECT COUNT(*) FROM apple_accounts", "account_issues": `SELECT COUNT(*) FROM apple_accounts WHERE (` + accountConnectionStatusSQL + `) IN ('failed','pending_2fa')`, "accounts_without_create_connection": "SELECT COUNT(*) FROM apple_accounts a WHERE NOT EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=a.id AND c.kind IN ('apple_account','icloud_web') AND c.status='healthy')", "accounts_without_imap": "SELECT COUNT(*) FROM apple_accounts a WHERE NOT EXISTS (SELECT 1 FROM account_connections c WHERE c.account_id=a.id AND c.kind='imap' AND c.status='healthy')", "mailboxes": "SELECT COUNT(*) FROM mailboxes", "available_mailboxes": "SELECT COUNT(*) FROM mailboxes WHERE status='available' AND allocation_mode='shared' AND local_enabled=1 AND remote_present=1 AND remote_enabled=1", "leased_mailboxes": "SELECT COUNT(*) FROM mailboxes WHERE status='leased'", "disabled_mailboxes": "SELECT COUNT(*) FROM mailboxes WHERE status='disabled' OR local_enabled=0 OR remote_enabled=0", "remote_missing_mailboxes": "SELECT COUNT(*) FROM mailboxes WHERE status='remote_missing' OR remote_present=0", "active_leases": "SELECT COUNT(*) FROM leases WHERE status='active' AND expires_at>?", "messages_24h": "SELECT COUNT(*) FROM messages WHERE received_at>?", "running_jobs": "SELECT COUNT(*) FROM jobs WHERE status IN ('queued','running','cancelling')", "failed_jobs": "SELECT COUNT(*) FROM jobs WHERE status='failed'", "healthy_imap_connections": "SELECT COUNT(*) FROM account_connections WHERE kind='imap' AND status='healthy'", "configured_imap_connections": "SELECT COUNT(*) FROM account_connections WHERE kind='imap'"}
	for key, q := range queries {
		var n int
		var err error
		if strings.Contains(q, "?") {
			cut := time.Now().UTC()
			if key == "messages_24h" {
				cut = cut.Add(-24 * time.Hour)
			}
			err = s.db.QueryRowContext(ctx, q, timeText(cut)).Scan(&n)
		} else {
			err = s.db.QueryRowContext(ctx, q).Scan(&n)
		}
		if err != nil {
			return nil, err
		}
		out[key] = n
	}
	var nextName, nextRun sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT name,next_run_at FROM schedules WHERE enabled=1 AND paused_reason='' AND next_run_at IS NOT NULL ORDER BY next_run_at LIMIT 1").Scan(&nextName, &nextRun); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out["next_schedule_name"] = nextName.String
	out["next_schedule_at"] = nextRun.String
	return out, nil
}

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return value, err
}
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, nowText())
	return err
}

func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key,value FROM settings ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) ListActivities(ctx context.Context, limit int) ([]domain.Activity, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,source,type,result,entity_type,entity_id,request_id,remote_ip,api_client_id,detail,created_at FROM activities ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Activity
	for rows.Next() {
		var e domain.Activity
		var created sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.Type, &e.Result, &e.EntityType, &e.EntityID, &e.RequestID, &e.RemoteIP, &e.APIClientID, &e.Detail, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListActivitiesPage(ctx context.Context, page, pageSize int) ([]domain.Activity, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM activities").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,source,type,result,entity_type,entity_id,request_id,remote_ip,api_client_id,detail,created_at FROM activities ORDER BY id DESC LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []domain.Activity
	for rows.Next() {
		var e domain.Activity
		var created sql.NullString
		if err := rows.Scan(&e.ID, &e.Source, &e.Type, &e.Result, &e.EntityType, &e.EntityID, &e.RequestID, &e.RemoteIP, &e.APIClientID, &e.Detail, &created); err != nil {
			return nil, 0, err
		}
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func (s *Store) UpdateActivity(ctx context.Context, id int64, typ, result, detail string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE activities SET type=?,result=?,detail=? WHERE id=?`, strings.TrimSpace(typ), strings.TrimSpace(result), strings.TrimSpace(detail), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteActivity(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM activities WHERE id=?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=CASE WHEN status='queued' THEN 'cancelled' ELSE 'cancelling' END,finished_at=CASE WHEN status='queued' THEN ? ELSE finished_at END WHERE id=? AND status IN ('queued','running')`, nowText(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
