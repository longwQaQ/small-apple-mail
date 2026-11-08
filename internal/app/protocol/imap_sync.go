package protocol

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func SyncICloudIMAPMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (map[string][]ICloudSyncedMessage, error) {
	result, err := SyncICloudIMAPMessagesWithCursor(ctx, state, mailboxes, after, keyword, maxMessages)
	if err != nil {
		return nil, err
	}
	return result.MessagesByMailbox, nil
}

func LatestICloudIMAPUID(ctx context.Context, state LoginState) (string, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return "", errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return "", errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return "", errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return "", errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return "", errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return "", errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return "", errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	uid := imapSelectLastUID(selectLines)
	if uid <= 0 {
		return "", nil
	}
	return strconv.Itoa(uid), nil
}

func SyncICloudIMAPMessagesWithCursor(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int) (iCloudIMAPSyncResult, error) {
	return syncICloudIMAPMessages(ctx, state, mailboxes, after, keyword, maxMessages, nil)
}

// SyncICloudIMAPPage 从已确认的 UID 向前分页，只有调用方保存邮件后才提交返回的游标。
func SyncICloudIMAPPage(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, cursor IMAPCursor) (IMAPSyncResult, error) {
	return syncICloudIMAPMessages(ctx, state, mailboxes, after, "", 200, &cursor)
}

func syncICloudIMAPMessages(ctx context.Context, state LoginState, mailboxes []Mailbox, after time.Time, keyword string, maxMessages int, cursor *IMAPCursor) (IMAPSyncResult, error) {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return iCloudIMAPSyncResult{}, err
	}
	if maxMessages <= 0 {
		maxMessages = 50
	}
	if maxMessages > 200 {
		maxMessages = 200
	}
	if keyword = strings.TrimSpace(keyword); keyword == "" {
		keyword = "OpenAI"
	}
	if len(mailboxes) == 0 {
		return iCloudIMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return iCloudIMAPSyncResult{}, errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return iCloudIMAPSyncResult{}, errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return iCloudIMAPSyncResult{}, errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	nextCursor := IMAPCursor{}
	if cursor != nil {
		validity := imapSelectUIDValidity(selectLines)
		if validity == "" {
			return IMAPSyncResult{}, fmt.Errorf("收件服务器未返回 UIDVALIDITY，无法安全保存同步进度")
		}
		nextCursor.UIDValidity = validity
		state.IMAPLastSyncUID = ""
		if validity == cursor.UIDValidity {
			state.IMAPLastSyncUID = cursor.LastUID
			nextCursor.LastUID = cursor.LastUID
		}
	}
	searchLines, err := imapCommand(conn, reader, "A003", imapSearchCommand(state, mailboxes, after))
	if err != nil {
		return iCloudIMAPSyncResult{}, errCode("imap_search_failed", "搜索 iCloud IMAP 邮件失败："+err.Error(), true)
	}
	if !imapTaggedOK(searchLines, "A003") {
		return iCloudIMAPSyncResult{}, errCode("imap_search_failed", "搜索 iCloud IMAP 邮件失败："+imapResponseSummary(searchLines), true)
	}
	uids := imapSearchUIDs(searchLines)
	sortInts(uids)
	hasMore := false
	if cursor != nil {
		uids, hasMore = imapForwardPage(uids, imapUIDNumber(state.IMAPLastSyncUID), maxMessages)
	} else {
		uids = lastIntValues(uids, maxMessages)
	}
	if len(uids) == 0 {
		_, _ = imapCommand(conn, reader, "A004", "LOGOUT")
		return IMAPSyncResult{MessagesByMailbox: map[string][]ICloudSyncedMessage{}, Cursor: nextCursor}, nil
	}
	lastUID := strconv.Itoa(uids[len(uids)-1])
	nextCursor.LastUID = lastUID

	fetched := make([]iCloudIMAPFetchedMessage, 0, len(uids))
	tag := 4
	for _, chunk := range chunkInts(uids, 20) {
		tag++
		lines, literals, err := imapCommandWithLiterals(conn, reader, fmt.Sprintf("A%03d", tag), "UID FETCH "+imapUIDSet(chunk)+" (UID BODY.PEEK[]<0.200000>)")
		if err != nil {
			return iCloudIMAPSyncResult{}, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件失败："+err.Error(), true)
		}
		if !imapTaggedOK(lines, fmt.Sprintf("A%03d", tag)) {
			return iCloudIMAPSyncResult{}, errCode("imap_fetch_failed", "读取 iCloud IMAP 邮件失败："+imapResponseSummary(lines), true)
		}
		fetchUIDs := imapFetchUIDs(lines)
		for i, raw := range literals {
			uid := ""
			if i < len(fetchUIDs) {
				uid = strconv.Itoa(fetchUIDs[i])
			}
			fetched = append(fetched, iCloudIMAPFetchedMessage{UID: uid, Raw: raw})
		}
	}
	_, _ = imapCommand(conn, reader, fmt.Sprintf("A%03d", tag+1), "LOGOUT")
	return iCloudIMAPSyncResult{
		MessagesByMailbox: iCloudIMAPMessagesByMailbox(fetched, mailboxes, after, keyword, state.IMAPEmail, state.IMAPUsername),
		LastUID:           lastUID,
		Cursor:            nextCursor, HasMore: hasMore,
	}, nil
}

func imapSelectUIDValidity(lines []string) string {
	for _, line := range lines {
		_, tail, ok := strings.Cut(strings.ToUpper(line), "[UIDVALIDITY ")
		if !ok {
			continue
		}
		value, _, ok := strings.Cut(tail, "]")
		if ok && imapUIDNumber(strings.TrimSpace(value)) > 0 {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func imapForwardPage(uids []int, last, limit int) ([]int, bool) {
	result := make([]int, 0, min(len(uids), limit+1))
	for _, uid := range uids {
		if uid > last {
			result = append(result, uid)
			if len(result) > limit {
				return result[:limit], true
			}
		}
	}
	return result, false
}
