package protocol

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MoveICloudIMAPMessagesToTrash 使用消息同步阶段保存的 IMAP UID，
// 将指定邮件从 INBOX 移入服务器声明为 \Trash 的文件夹，不执行永久删除。
func MoveICloudIMAPMessagesToTrash(ctx context.Context, state LoginState, remoteIDs []string) (ICloudMailCleanupResult, error) {
	var result ICloudMailCleanupResult
	uids, skipped := imapCleanupUIDs(remoteIDs)
	result.Skipped = skipped
	if len(uids) == 0 {
		return result, nil
	}
	if state.IMAPUIDValidity == "" {
		return result, errCode("imap_sync_required", "缺少收件箱同步信息，请先同步收件后再清理", false)
	}
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return result, errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	moved, err := moveICloudIMAPUIDsToTrash(conn, state, uids)
	result.MovedToTrash = moved
	return result, err
}

func moveICloudIMAPUIDsToTrash(conn net.Conn, state LoginState, uids []int) (int, error) {
	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return 0, errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return 0, errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil || !imapTaggedOK(loginLines, "A001") {
		return 0, errCode("imap_login_failed", "iCloud IMAP 登录失败："+imapResponseSummary(loginLines), false)
	}
	listLines, err := imapCommand(conn, reader, "A002", `LIST "" "*"`)
	if err != nil || !imapTaggedOK(listLines, "A002") {
		return 0, errCode("imap_list_failed", "读取 iCloud IMAP 文件夹失败："+imapResponseSummary(listLines), true)
	}
	trash, ok := imapTrashMailbox(listLines)
	if !ok {
		return 0, errCode("imap_trash_not_found", "未找到 iCloud IMAP 废纸篓文件夹", true)
	}
	selectLines, err := imapCommand(conn, reader, "A003", "SELECT INBOX")
	if err != nil || !imapTaggedOK(selectLines, "A003") {
		return 0, errCode("imap_select_failed", "打开 iCloud IMAP 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	if imapSelectUIDValidity(selectLines) != state.IMAPUIDValidity {
		return 0, errCode("imap_sync_required", "收件箱 UID 代次已变化，请重新同步收件后再清理", false)
	}
	moved := 0
	tag := 4
	for _, chunk := range chunkInts(uids, 200) {
		currentTag := fmt.Sprintf("A%03d", tag)
		tag++
		lines, commandErr := imapCommand(conn, reader, currentTag, "UID MOVE "+imapUIDSet(chunk)+" "+imapQuote(trash))
		if commandErr != nil || !imapTaggedOK(lines, currentTag) {
			return moved, errCode("imap_move_failed", "移动 iCloud IMAP 邮件到废纸篓失败："+imapResponseSummary(lines), true)
		}
		moved += len(chunk)
	}
	_, _ = imapCommand(conn, reader, fmt.Sprintf("A%03d", tag), "LOGOUT")
	return moved, nil
}

func imapCleanupUIDs(remoteIDs []string) ([]int, int) {
	seen := make(map[int]struct{}, len(remoteIDs))
	uids := make([]int, 0, len(remoteIDs))
	skipped := 0
	for _, remoteID := range remoteIDs {
		raw, ok := strings.CutPrefix(strings.TrimSpace(remoteID), "imap:")
		uid, err := strconv.Atoi(raw)
		if !ok || err != nil || uid <= 0 {
			skipped++
			continue
		}
		if _, exists := seen[uid]; exists {
			continue
		}
		seen[uid] = struct{}{}
		uids = append(uids, uid)
	}
	sort.Ints(uids)
	return uids, skipped
}

func imapTrashMailbox(lines []string) (string, bool) {
	for _, line := range lines {
		if !strings.Contains(strings.ToUpper(line), `\TRASH`) {
			continue
		}
		quoted := imapQuotedValues(line)
		if len(quoted) > 0 && strings.TrimSpace(quoted[len(quoted)-1]) != "" {
			return quoted[len(quoted)-1], true
		}
	}
	return "", false
}

func imapQuotedValues(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		i++
		var value strings.Builder
		for i < len(line) {
			if line[i] == '\\' && i+1 < len(line) {
				i++
				value.WriteByte(line[i])
				i++
				continue
			}
			if line[i] == '"' {
				out = append(out, value.String())
				break
			}
			value.WriteByte(line[i])
			i++
		}
	}
	return out
}
