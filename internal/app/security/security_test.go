package security

import (
	"strings"
	"testing"
)

func TestPasswordHashAndValidation(t *testing.T) {
	password := "Correct-Horse-电池-Staple-2026"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "argon2id$v=19$") || !VerifyPassword(password, hash) {
		t.Fatalf("密码哈希无法验证: %q", hash)
	}
	if VerifyPassword(password+"x", hash) || VerifyPassword(password, "invalid") {
		t.Fatal("错误密码或无效哈希不应通过验证")
	}
	if ValidatePassword("1234567") == nil || ValidatePassword(strings.Repeat("密", 33)) == nil {
		t.Fatal("密码长度边界未生效")
	}
}

func TestPasswordAllowsEightToThirtyTwoCharacters(t *testing.T) {
	for _, password := range []string{"12345678", strings.Repeat("密", 32)} {
		hash, err := HashPassword(password)
		if err != nil || !VerifyPassword(password, hash) {
			t.Fatalf("password length=%d hash=%q err=%v", len([]rune(password)), hash, err)
		}
	}
	for _, password := range []string{"1234567", strings.Repeat("x", 33)} {
		if _, err := HashPassword(password); err == nil {
			t.Fatalf("password length=%d 应被拒绝", len([]rune(password)))
		}
	}
}

func TestTokenHashAndRandomPrefixes(t *testing.T) {
	token, err := RandomToken("sam_live_", 24)
	if err != nil || !strings.HasPrefix(token, "sam_live_") {
		t.Fatalf("token=%q err=%v", token, err)
	}
	id, err := ID("job")
	if err != nil || !strings.HasPrefix(id, "job_") {
		t.Fatalf("id=%q err=%v", id, err)
	}
	hash := HashToken("  secret  ")
	if hash == "secret" || !EqualHash("secret", hash) || EqualHash("different", hash) {
		t.Fatal("Token 哈希比较不符合预期")
	}
}

func TestCSRFTokenIsStableAndSessionSpecific(t *testing.T) {
	first := CSRFToken(HashToken("session-one"))
	if first == "" || first != CSRFToken(HashToken("session-one")) {
		t.Fatalf("同一会话的 CSRF Token 不稳定: %q", first)
	}
	if first == CSRFToken(HashToken("session-two")) {
		t.Fatal("不同会话不应共享 CSRF Token")
	}
}
