package protocol

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestIMAPForwardPageDoesNotSkipOlderBacklog(t *testing.T) {
	uids := make([]int, 501)
	for i := range uids {
		uids[i] = i + 1
	}
	first, more := imapForwardPage(uids, 0, 200)
	if !more || len(first) != 200 || first[0] != 1 || first[199] != 200 {
		t.Fatal(first, more)
	}
	next, more := imapForwardPage(uids, 200, 200)
	if !more || len(next) != 200 || next[0] != 201 || next[199] != 400 {
		t.Fatal(next, more)
	}
	last, more := imapForwardPage(uids, 400, 200)
	if more || len(last) != 101 || last[0] != 401 {
		t.Fatal(last, more)
	}
	empty, more := imapForwardPage([]int{501}, 501, 200)
	if len(empty) != 0 || more {
		t.Fatal("IMAP UID 范围返回的旧末项必须排除", empty)
	}
	if value := imapSelectUIDValidity([]string{"* OK [UIDVALIDITY 987] valid"}); value != "987" {
		t.Fatal(value)
	}
	if value := imapSelectUIDValidity([]string{"* OK [UIDVALIDITY nope]"}); value != "" {
		t.Fatal(value)
	}
}

func TestIMAPCleanupRequiresMatchingUIDValidity(t *testing.T) {
	if _, err := MoveICloudIMAPMessagesToTrash(context.Background(), LoginState{}, []string{"imap:42"}); !isCodedError(err, "imap_sync_required") {
		t.Fatal("缺少同步信息时应在连接服务器前拒绝清理", err)
	}
	for _, validity := range []string{"123", ""} {
		t.Run("server-"+validity, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			_ = server.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan error, 1)
			go func() {
				reader := bufio.NewReader(server)
				fmt.Fprint(server, "* OK ready\r\n")
				for _, step := range []struct{ command, response string }{
					{`A001 LOGIN "owner@example.invalid" "fixture"`, "A001 OK\r\n"},
					{`A002 LIST "" "*"`, "* LIST (\\Trash) \"/\" \"Trash\"\r\nA002 OK\r\n"},
					{"A003 SELECT INBOX", "* OK [UIDVALIDITY " + validity + "]\r\nA003 OK\r\n"},
				} {
					line, err := reader.ReadString('\n')
					if err != nil || strings.TrimSpace(line) != step.command {
						done <- fmt.Errorf("command=%q want=%q err=%v", line, step.command, err)
						return
					}
					fmt.Fprint(server, step.response)
				}
				line, err := reader.ReadString('\n')
				if err != io.EOF || line != "" {
					done <- fmt.Errorf("代次不匹配后仍发送命令: %q %v", line, err)
					return
				}
				done <- nil
			}()
			moved, err := moveICloudIMAPUIDsToTrash(client, LoginState{IMAPUsername: "owner@example.invalid", IMAPAppPassword: "fixture", IMAPUIDValidity: "987"}, []int{42})
			client.Close()
			if moved != 0 || !isCodedError(err, "imap_sync_required") {
				t.Fatal(moved, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
