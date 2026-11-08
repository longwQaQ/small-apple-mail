package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestIMAPRecipientMatchingRejectsUnrelatedAddresses(t *testing.T) {
	cases := []struct {
		name, headers string
		match         bool
	}{
		{"收件人", "To: target@icloud.com\r\n", true},
		{"GB2312 显示名", "To: =?GB2312?B?suLK1A==?= <target@icloud.com>\r\n", true},
		{"GBK 显示名", "To: =?GBK?B?suLK1A==?= <target@icloud.com>\r\n", true},
		{"抄送", "To: other@example.com\r\nCc: target@icloud.com\r\n", true},
		{"转发原收件人", "To: owner@icloud.com\r\nX-Original-To: target@icloud.com\r\n", true},
		{"主题中提到地址", "To: other@example.com\r\nSubject: target@icloud.com\r\n", false},
		{"发件人地址", "From: target@icloud.com\r\nTo: other@example.com\r\n", false},
		{"地址前缀不同", "To: nottarget@icloud.com\r\n", false},
		{"地址域名不同", "To: target@icloud.com.example.com\r\n", false},
		{"收件人显示名", "To: \"target@icloud.com\" <other@example.com>\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.headers + "Content-Type: text/plain; charset=utf-8\r\n\r\n验证码 123456"
			rows := iCloudIMAPMessagesByMailbox([]iCloudIMAPFetchedMessage{{UID: "1", Raw: []byte(raw)}}, []Mailbox{{ID: "target", Email: "target@icloud.com"}}, time.Time{}, "")
			if got := len(rows["target"]) > 0; got != tc.match {
				t.Fatalf("邮件归属错误: matched=%v want=%v", got, tc.match)
			}
		})
	}
}

func TestWebMailRecipientMetadata(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"other@icloud.com","email":"target@icloud.com"}]`,
		`["\"other@icloud.com\" <target@icloud.com>"]`,
		`"target@icloud.com"`,
		`{"name":"User","email":"target@icloud.com"}`,
	} {
		got := matchingMailboxIDs(mailMetadataRecipients(json.RawMessage(raw)), map[string]string{"target": "target@icloud.com", "other": "other@icloud.com"})
		if !reflect.DeepEqual(got, []string{"target"}) {
			t.Fatalf("元数据收件人匹配错误: raw=%s matched=%v", raw, got)
		}
	}
}
