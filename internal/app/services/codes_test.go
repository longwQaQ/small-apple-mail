package services

import "testing"

func TestExtractCode(t *testing.T) {
	tests := []struct{ subject, body, want string }{
		{"Your verification code", "Code: 246810", "246810"},
		{"登录验证码 7733", "", "7733"},
		{"Newsletter", "Order 123456789", ""},
		{"ChatGPT Business admin update: slides, data plugin, and chrome extension", ".links-000000-underline a { color: #000000; text-decoration: underline; }", ""},
		{"Your verification code", "Your code is 000000", "000000"},
		{"Newsletter", "Text color: #123456; reference ABC654321DEF", ""},
		{"", "123456", "123456"},
		{"登录", "(654321)", "654321"},
		{"登录", "123456是您的验证码，请勿泄露", "123456"},
		{"登录", "请使用123456完成登录", "123456"},
		{"登录", "请访问https://example.com，验证码：123456", "123456"},
		{"登录", "请访问https://example.com/path?token=654321。验证码：123456", "123456"},
		{"登录", "https://claude.ai/magic-link#synthetic-123456:example==", ""},
	}
	for _, tt := range tests {
		if got := ExtractCode(tt.subject, tt.body); got.Code != tt.want {
			t.Fatalf("ExtractCode(%q,%q)=%q want %q", tt.subject, tt.body, got.Code, tt.want)
		}
	}
}
