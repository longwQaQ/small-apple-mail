package services

import "testing"

func TestExtractClaudeLoginURL(t *testing.T) {
	const link = "https://claude.ai/magic-link#test-123456:dGVzdEBleGFtcGxlLmNvbQ=="
	for _, tc := range []struct{ name, body, want string }{
		{"HTML 按钮", `<a href="` + link + `">Sign in</a><img src="https://claude.ai/images/email/logo.png">`, link},
		{"纯文本", "Sign in: " + link + "\nThis link expires in 10 minutes.", link},
		{"中文说明紧接链接", "登录链接：" + link + "。有效期10分钟。", link},
		{"纯文本参数", "https://claude.ai/magic-link?token=test&notifier=1", "https://claude.ai/magic-link?token=test&notifier=1"},
		{"HTML 参数", `<a href="https://claude.ai/magic-link?token=test&notifier=1">Sign in</a>`, "https://claude.ai/magic-link?token=test&notifier=1"},
		{"HTML 链接末尾标点", `<a href="https://claude.ai/magic-link#test;">Sign in</a>`, "https://claude.ai/magic-link#test;"},
		{"重复链接", link + "\n" + link, link},
		{"HTML 实体", `<a href="https://claude.ai/magic-link?token=test&amp;source=email">Sign in</a>`, "https://claude.ai/magic-link?token=test&source=email"},
		{"跟踪链接", "https://url8792.mail.anthropic.com/ls/click?upn=test", ""},
		{"图片及普通页面", "https://claude.ai/images/email/logo.png https://claude.ai/", ""},
		{"仿冒域名", "https://claude.ai.example.com/magic-link#test", ""},
		{"用户信息", "https://claude.ai@evil.example/magic-link#test", ""},
		{"错误协议", "http://claude.ai/magic-link#test", ""},
		{"缺少凭据", "https://claude.ai/magic-link", ""},
		{"不同登录链接", link + " https://claude.ai/magic-link#other", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractClaudeLoginURL(tc.body); got != tc.want {
				t.Fatal("登录链接提取结果不符合预期")
			}
		})
	}
	if got := ExtractCode("Your secure link to Claude is here", "Sign in: "+link); got.Code != "" {
		t.Fatal("登录链接中的数字不能识别为验证码")
	}
}
