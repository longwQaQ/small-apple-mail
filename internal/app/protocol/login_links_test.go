package protocol

import (
	"bytes"
	"encoding/base64"
	"mime/quotedprintable"
	"strings"
	"testing"

	"small-apple-mail/internal/app/services"
)

func TestIMAPExtractsClaudeLoginLinkBeforeRemovingHTML(t *testing.T) {
	const link = "https://claude.ai/magic-link#test-123456:dGVzdEBleGFtcGxlLmNvbQ=="
	body := `<html><img src="https://claude.ai/images/email/logo.png"><a href="` + link + `">Sign in</a><img src="https://url8792.mail.anthropic.com/wf/open?upn=tracking"></html>`
	var encoded bytes.Buffer
	w := quotedprintable.NewWriter(&encoded)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ encoding, body string }{
		{"quoted-printable", encoded.String()},
		{"base64", base64.StdEncoding.EncodeToString([]byte(body))},
	} {
		t.Run(tc.encoding, func(t *testing.T) {
			raw := "To: alias@icloud.com\r\nSubject: Your secure link to Claude is here\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: " + tc.encoding + "\r\n\r\n" + tc.body
			message, _, ok := parseICloudIMAPMessage(iCloudIMAPFetchedMessage{UID: "1", Raw: []byte(raw)})
			if !ok || message.LoginURL != link {
				t.Fatal("未完整保留登录链接及 fragment")
			}
			if strings.Contains(message.Body, "<img") || strings.Contains(message.Body, "<a") {
				t.Fatal("邮件正文未转换为文本")
			}
		})
	}
}

func TestIMAPLoginLinkIgnoresAttachments(t *testing.T) {
	const link = "https://claude.ai/magic-link#synthetic:body=="
	raw := strings.Join([]string{
		"To: alias@icloud.com",
		"Subject: Your secure link to Claude is here",
		`Content-Type: multipart/mixed; boundary="test-mail"`,
		"",
		"--test-mail",
		"Content-Type: text/html; charset=UTF-8",
		"",
		`<a href="` + link + `">Sign in</a>`,
		"--test-mail",
		"Content-Type: text/plain",
		`Content-Disposition: attachment; filename="old-mail.txt"`,
		"",
		"https://claude.ai/magic-link#synthetic:attachment== Verification code: 123456",
		"--test-mail",
		"Content-Type: application/octet-stream",
		"Content-Transfer-Encoding: base64",
		"",
		base64.StdEncoding.EncodeToString([]byte("https://claude.ai/magic-link#synthetic:binary== Verification code: 654321")),
		"--test-mail--",
		"",
	}, "\r\n")
	message, _, ok := parseICloudIMAPMessage(iCloudIMAPFetchedMessage{UID: "2", Raw: []byte(raw)})
	if !ok || message.LoginURL != link {
		t.Fatal("附件中的链接不应影响正文登录链接")
	}
	if services.ExtractCode(message.Subject, message.Body).Code != "" || strings.Contains(message.Body, "attachment") || strings.Contains(message.Body, "binary") {
		t.Fatal("附件内容不应作为邮件正文参与提取")
	}
}
