package protocol

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"small-apple-mail/internal/app/services"
)

func imapSearchCommand(state LoginState, mailboxes []Mailbox, after time.Time) string {
	if uid := imapUIDNumber(state.IMAPLastSyncUID); uid > 0 {
		return "UID SEARCH UID " + strconv.Itoa(uid+1) + ":*"
	}
	nextUID := 0
	for _, mailbox := range mailboxes {
		uid := imapUIDNumber(mailbox.LastSyncUID)
		if uid <= 0 {
			nextUID = 0
			break
		}
		if nextUID == 0 || uid+1 < nextUID {
			nextUID = uid + 1
		}
	}
	if nextUID > 0 {
		return "UID SEARCH UID " + strconv.Itoa(nextUID) + ":*"
	}
	searchAfter := after
	if searchAfter.IsZero() {
		searchAfter = time.Now().Add(-24 * time.Hour)
	}
	return "UID SEARCH SINCE " + searchAfter.Format("2-Jan-2006")
}

func imapSelectLastUID(lines []string) int {
	for _, line := range lines {
		upper := strings.ToUpper(line)
		idx := strings.Index(upper, "UIDNEXT")
		if idx < 0 {
			continue
		}
		tail := line[idx+len("UIDNEXT"):]
		fields := strings.FieldsFunc(tail, func(r rune) bool {
			return r < '0' || r > '9'
		})
		if len(fields) == 0 {
			continue
		}
		nextUID, err := strconv.Atoi(fields[0])
		if err == nil && nextUID > 1 {
			return nextUID - 1
		}
	}
	return 0
}

func imapUIDNumber(value string) int {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "imap:")
	uid, err := strconv.Atoi(value)
	if err != nil || uid <= 0 {
		return 0
	}
	return uid
}

func imapLineHasExistsEvent(line string) bool {
	parts := strings.Fields(strings.TrimSpace(line))
	return len(parts) >= 3 && parts[0] == "*" && strings.EqualFold(parts[2], "EXISTS")
}

type iCloudIMAPFetchedMessage struct {
	UID string
	Raw []byte
}

func iCloudIMAPMessagesByMailbox(fetched []iCloudIMAPFetchedMessage, mailboxes []Mailbox, after time.Time, keyword string, ignoredEmails ...string) map[string][]ICloudSyncedMessage {
	now := time.Now()
	aliases := make(map[string]string)
	afterByMailbox := make(map[string]time.Time)
	ignored := make(map[string]struct{}, len(ignoredEmails))
	for _, email := range ignoredEmails {
		if email = normalizeICloudIMAPEmail(email); email != "" {
			ignored[email] = struct{}{}
		}
	}
	for _, mailbox := range mailboxes {
		id := strings.TrimSpace(mailbox.ID)
		email := normalizeICloudIMAPEmail(mailbox.Email)
		if id == "" || email == "" {
			continue
		}
		if _, skip := ignored[email]; skip {
			continue
		}
		aliases[id] = email
		afterByMailbox[id] = mailboxSyncAfter(mailbox, after, now)
	}
	out := make(map[string][]ICloudSyncedMessage, len(aliases))
	for _, item := range fetched {
		message, recipients, ok := parseICloudIMAPMessage(item)
		if !ok {
			continue
		}
		matchedMailboxIDs := matchingMailboxIDs(recipients, aliases)
		if len(matchedMailboxIDs) == 0 {
			continue
		}
		for _, mailboxID := range matchedMailboxIDs {
			if after := afterByMailbox[mailboxID]; !after.IsZero() && message.ReceivedAt.Before(after) {
				continue
			}
			out[mailboxID] = append(out[mailboxID], message)
		}
	}
	for mailboxID := range out {
		sortMessagesByReceivedAt(out[mailboxID])
	}
	return out
}

func parseICloudIMAPMessage(item iCloudIMAPFetchedMessage) (ICloudSyncedMessage, string, bool) {
	msg, err := mail.ReadMessage(bytes.NewReader(item.Raw))
	if err != nil {
		return ICloudSyncedMessage{}, "", false
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(msg.Body, 1<<20))
	body := decodeICloudIMAPBody(msg.Header, bodyBytes)
	subject := decodeMIMEHeader(msg.Header.Get("Subject"))
	from := decodeMIMEHeader(msg.Header.Get("From"))
	receivedAt := time.Now()
	if date, err := msg.Header.Date(); err == nil {
		receivedAt = date
	}
	uid := strings.TrimSpace(item.UID)
	remoteID := "imap"
	if uid != "" {
		remoteID += ":" + uid
	}
	recipients := mailRecipientText(msg.Header)
	return ICloudSyncedMessage{
		RemoteID:   remoteID,
		UID:        uid,
		Subject:    subject,
		From:       from,
		Body:       normalizeMailBody(subject + "\n" + body),
		LoginURL:   services.ExtractClaudeLoginURL(body),
		ReceivedAt: receivedAt,
	}, recipients, true
}

func normalizeICloudIMAPState(state LoginState) (LoginState, error) {
	email := normalizeICloudIMAPEmail(state.IMAPEmail)
	if email == "" {
		email = normalizeICloudIMAPEmail(state.IMAPUsername)
	}
	if email == "" {
		return LoginState{}, errCode("imap_session_missing", "未保存取码登录，请先保存 iCloud 邮箱账号和 App 专用密码", true)
	}
	appPassword := strings.TrimSpace(state.IMAPAppPassword)
	if appPassword == "" {
		return LoginState{}, errCode("imap_app_password_missing", "取码登录缺少 App 专用密码，请重新保存取码登录", false)
	}
	state.IMAPEmail = email
	state.IMAPUsername = firstNonEmpty(strings.TrimSpace(state.IMAPUsername), email)
	state.IMAPHost = firstNonEmpty(strings.TrimSpace(state.IMAPHost), defaultICloudIMAPHost)
	if state.IMAPPort == 0 {
		state.IMAPPort = defaultICloudIMAPPort
	}
	state.IMAPAppPassword = appPassword
	return state, nil
}

func imapLiteralSize(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	start := strings.LastIndex(line, "{")
	if start < 0 || start+1 >= len(line) {
		return 0, false
	}
	value := strings.TrimSuffix(line[start+1:], "}")
	value = strings.TrimSuffix(value, "+")
	n, err := strconv.Atoi(value)
	return n, err == nil && n >= 0
}

func imapSearchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "* SEARCH") {
			continue
		}
		for _, part := range strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "* SEARCH"))) {
			n, err := strconv.Atoi(part)
			if err == nil && n > 0 {
				out = append(out, n)
			}
		}
	}
	return out
}

func imapFetchUIDs(lines []string) []int {
	var out []int
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if strings.EqualFold(strings.Trim(fields[i], "()"), "UID") {
				value := strings.Trim(fields[i+1], "()")
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func lastIntValues(values []int, limit int) []int {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[len(values)-limit:]
}

func chunkInts(values []int, size int) [][]int {
	if size <= 0 {
		size = len(values)
	}
	var out [][]int
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}

func imapUIDSet(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func mailRecipientText(header mail.Header) string {
	// 这里只提取地址，不使用显示名；未知显示名字符集不应导致有效收件地址丢失。
	parser := mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}}}
	var out []string
	for key, values := range header {
		switch strings.ToLower(key) {
		case "to", "cc", "bcc", "delivered-to", "x-original-to", "envelope-to", "x-envelope-to", "resent-to", "resent-cc", "resent-bcc":
		default:
			continue
		}
		for _, value := range values {
			addresses, err := parser.ParseList(value)
			if err != nil {
				continue
			}
			for _, address := range addresses {
				out = append(out, address.Address)
			}
		}
	}
	return strings.Join(out, "\n")
}

func decodeMIMEHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decoded)
}

func decodeICloudIMAPBody(header mail.Header, body []byte) string {
	disposition, _, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	if strings.EqualFold(disposition, "attachment") {
		return ""
	}
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(header.Get("Content-Type")))
	}
	decoded := decodeICloudIMAPTransfer(header.Get("Content-Transfer-Encoding"), body)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return string(decoded)
		}
		reader := multipart.NewReader(bytes.NewReader(decoded), boundary)
		var parts []string
		for i := 0; i < 30; i++ {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			partBody, _ := io.ReadAll(io.LimitReader(part, 1<<20))
			text := decodeICloudIMAPBody(mail.Header(part.Header), partBody)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	if mediaType == "text/plain" || mediaType == "text/html" || mediaType == "" {
		return string(decoded)
	}
	return ""
}

func decodeICloudIMAPTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
		if err == nil {
			return decoded
		}
	}
	return body
}

func sortMessagesByReceivedAt(messages []ICloudSyncedMessage) {
	for i := 1; i < len(messages); i++ {
		for j := i; j > 0 && messages[j].ReceivedAt.After(messages[j-1].ReceivedAt); j-- {
			messages[j], messages[j-1] = messages[j-1], messages[j]
		}
	}
}
