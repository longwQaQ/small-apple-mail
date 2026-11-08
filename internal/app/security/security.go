package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory      = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 2
)

func RandomToken(prefix string, bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func ID(prefix string) (string, error) { return RandomToken(prefix+"_", 16) }

func HashToken(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// CSRFToken 从不可逆的 Session Token 哈希派生稳定的 CSRF Token。
// 同一会话的多个浏览器标签页会得到相同值；跨站页面无法读取 HttpOnly Cookie 来推导它。
func CSRFToken(sessionTokenHash string) string {
	sum := sha256.Sum256([]byte("csrf:" + strings.TrimSpace(sessionTokenHash)))
	return "csrf_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func EqualHash(value, wantHash string) bool {
	got := HashToken(value)
	return subtle.ConstantTimeCompare([]byte(got), []byte(wantHash)) == 1
}

func ValidatePassword(password string) error {
	n := len([]rune(password))
	if n < 8 || n > 32 {
		return fmt.Errorf("管理员密码必须为 8-32 个字符")
	}
	return nil
}

func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, passwordIterations, passwordMemory, passwordParallelism, 32)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordIterations, passwordParallelism,
		base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(key)), nil
}

func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	if memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 16 {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
