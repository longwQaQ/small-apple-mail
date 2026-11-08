package providers

import (
	"errors"
	"strings"
	"testing"
)

func TestConnectionFailureMessage(t *testing.T) {
	if got := connectionFailureMessage(nil); got != "连接检测失败" {
		t.Fatalf("nil error message = %q", got)
	}
	if got := connectionFailureMessage(errors.New("不支持的连接类型")); got != "连接检测失败：不支持的连接类型" {
		t.Fatalf("detailed error message = %q", got)
	}

	got := []rune(connectionFailureMessage(errors.New(strings.Repeat("错", 400))))
	if len(got) != 300 || got[len(got)-1] != '…' {
		t.Fatalf("long error message should be truncated to 300 runes, length=%d", len(got))
	}
}

func TestIMAPWatchFailureMessage(t *testing.T) {
	if got := imapWatchFailureMessage(errors.New("read tcp: i/o timeout")); got != "IMAP 实时监听中断，正在重连：read tcp: i/o timeout" {
		t.Fatalf("watch failure message = %q", got)
	}
	if got := []rune(imapWatchFailureMessage(errors.New(strings.Repeat("错", 400)))); len(got) != 300 || got[len(got)-1] != '…' {
		t.Fatalf("long watch failure message should be truncated to 300 runes, length=%d", len(got))
	}
}
