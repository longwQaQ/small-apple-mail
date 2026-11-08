package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"small-apple-mail/internal/app/services"
)

func (c *ICloudClient) SyncMailboxMessages(ctx context.Context, session ICloudSession, mailbox Mailbox, after time.Time, keyword string, maxThreads int) ([]ICloudSyncedMessage, error) {
	if strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return nil, errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	if strings.TrimSpace(mailbox.Email) == "" {
		return nil, errCode("mailbox_email_missing", "邮箱地址为空", false)
	}
	if maxThreads <= 0 || maxThreads > 50 {
		maxThreads = 20
	}
	queryAfter := mailboxSyncAfter(mailbox, after, time.Now())
	folders, err := c.mailFolders(ctx, session)
	if err != nil {
		return nil, err
	}
	folders = preferredMailFolders(folders)
	var out []ICloudSyncedMessage
	for _, folder := range folders {
		threads, err := c.searchThreads(ctx, session, folder, maxThreads)
		if err != nil {
			return out, err
		}
		for _, thread := range threads {
			if shouldSkipSyncedThread(thread, queryAfter) {
				continue
			}
			messages, err := c.threadMessages(ctx, session, folder, thread.ThreadID, mailbox.Email, queryAfter)
			if err != nil {
				return out, err
			}
			out = append(out, messages...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ReceivedAt.After(out[j].ReceivedAt)
	})
	return out, nil
}

func (c *ICloudClient) SyncMailboxMessagesBatch(ctx context.Context, session ICloudSession, mailboxes []Mailbox, after time.Time, keyword string, maxThreads int) (map[string][]ICloudSyncedMessage, error) {
	if strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return nil, errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	if maxThreads <= 0 || maxThreads > 50 {
		maxThreads = 50
	}
	now := time.Now()
	aliases := make(map[string]string)
	afterByMailbox := make(map[string]time.Time)
	var queryAfter time.Time
	for _, mailbox := range mailboxes {
		email := strings.ToLower(strings.TrimSpace(mailbox.Email))
		if strings.TrimSpace(mailbox.ID) == "" || email == "" {
			continue
		}
		aliases[mailbox.ID] = email
		mailboxAfter := mailboxSyncAfter(mailbox, after, now)
		afterByMailbox[mailbox.ID] = mailboxAfter
		if queryAfter.IsZero() || mailboxAfter.Before(queryAfter) {
			queryAfter = mailboxAfter
		}
	}
	if len(aliases) == 0 {
		return map[string][]ICloudSyncedMessage{}, nil
	}
	folders, err := c.mailFolders(ctx, session)
	if err != nil {
		return nil, err
	}
	folders = preferredMailFolders(folders)
	out := make(map[string][]ICloudSyncedMessage, len(aliases))
	for _, folder := range folders {
		threads, err := c.searchThreads(ctx, session, folder, maxThreads)
		if err != nil {
			return out, err
		}
		for _, thread := range threads {
			if shouldSkipSyncedThread(thread, queryAfter) {
				continue
			}
			messagesByMailbox, err := c.threadMessagesForAliases(ctx, session, folder, thread.ThreadID, aliases, afterByMailbox)
			if err != nil {
				return out, err
			}
			for mailboxID, messages := range messagesByMailbox {
				out[mailboxID] = append(out[mailboxID], messages...)
			}
		}
	}
	for mailboxID := range out {
		sort.SliceStable(out[mailboxID], func(i, j int) bool {
			return out[mailboxID][i].ReceivedAt.After(out[mailboxID][j].ReceivedAt)
		})
	}
	return out, nil
}

func mailboxSyncAfter(mailbox Mailbox, after time.Time, now time.Time) time.Time {
	queryAfter := after
	if !mailbox.LastSyncAt.IsZero() {
		cursor := mailbox.LastSyncAt.Add(-mailboxSyncCursorOverlap)
		if cursor.After(queryAfter) {
			queryAfter = cursor
		}
	}
	if queryAfter.After(now) {
		return now.Add(-mailboxSyncCursorOverlap)
	}
	return queryAfter
}

func shouldSkipSyncedThread(thread mailThread, after time.Time) bool {
	return !after.IsZero() && !thread.ReceivedAt.IsZero() && thread.ReceivedAt.Before(after)
}

func looksLikeVerificationText(text, keyword string) bool {
	if extractOTP(text) != "" {
		return true
	}
	needles := []string{"openai", "chatgpt", "code", "otp", "verification", "verify", "验证码", "验证", "代码"}
	if strings.TrimSpace(keyword) != "" {
		needles = append(needles, strings.ToLower(strings.TrimSpace(keyword)))
	}
	lower := strings.ToLower(text)
	for _, needle := range needles {
		if needle != "" && strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func (c *ICloudClient) CheckMailSession(ctx context.Context, session ICloudSession) error {
	if strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	if _, err := mailGatewayBaseURL(session); err != nil {
		return err
	}
	_, err := c.mailFolders(ctx, session)
	return err
}

func (c *ICloudClient) mailFolders(ctx context.Context, session ICloudSession) ([]mailFolder, error) {
	var out struct {
		DomainObjects []mailFolder `json:"domainObjects"`
	}
	body := map[string]any{
		"domain":        "mailbox",
		"includeLabels": true,
		"predicate": map[string]any{
			"type": "eq",
			"expression": map[string]any{
				"type":     "property",
				"property": "isMboxDeleted",
			},
			"value": false,
		},
		"properties": []string{"identifier", "name", "uidValidity", "unseenCount", "seenDeletedCount", "unseenDeletedCount", "messageCount", "flags"},
	}
	if err := c.callMail(ctx, session, "/mailws2/v1/geqs/query", body, "fetchMailboxCountQuery", &out); err != nil {
		return nil, err
	}
	return out.DomainObjects, nil
}

func preferredMailFolders(folders []mailFolder) []mailFolder {
	if len(folders) == 0 {
		return []mailFolder{{Name: "INBOX"}}
	}
	var out []mailFolder
	seen := map[string]bool{}
	add := func(folder mailFolder) {
		name := strings.TrimSpace(folder.Name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, folder)
	}
	for _, folder := range folders {
		if folder.Name == "INBOX" || strings.HasPrefix(folder.Name, "INBOX/$category$_") {
			add(folder)
		}
	}
	for _, folder := range folders {
		if folder.MessageCount > 0 && !strings.Contains(folder.Name, "Deleted") && folder.Name != "Sent Messages" && folder.Name != "Drafts" {
			add(folder)
		}
	}
	if len(out) == 0 {
		out = append(out, mailFolder{Name: "INBOX"})
	}
	return out
}

func (c *ICloudClient) searchThreads(ctx context.Context, session ICloudSession, folder mailFolder, maxThreads int) ([]mailThread, error) {
	var out struct {
		ThreadList []struct {
			ThreadID  string          `json:"threadId"`
			Subject   string          `json:"subject"`
			Preview   string          `json:"preview"`
			Timestamp json.RawMessage `json:"timestamp"`
		} `json:"threadList"`
	}
	body := map[string]any{
		"responseType":        "THREAD_DIGEST",
		"includeFolderStatus": false,
		"maxResults":          maxThreads,
		"sessionHeaders":      mailSessionHeaders(folder.Name, false),
	}
	if err := c.callMail(ctx, session, "/mailws2/v1/thread/search", body, "", &out); err != nil {
		return nil, err
	}
	threads := make([]mailThread, 0, len(out.ThreadList))
	for _, item := range out.ThreadList {
		threads = append(threads, mailThread{
			ThreadID:   item.ThreadID,
			Subject:    item.Subject,
			Preview:    item.Preview,
			ReceivedAt: parseMailTime(item.Timestamp),
		})
	}
	return threads, nil
}

func (c *ICloudClient) threadMessages(ctx context.Context, session ICloudSession, folder mailFolder, threadID, alias string, after time.Time) ([]ICloudSyncedMessage, error) {
	messagesByAlias, err := c.threadMessagesForAliases(ctx, session, folder, threadID, map[string]string{"single": alias}, map[string]time.Time{"single": after})
	if err != nil {
		return nil, err
	}
	return messagesByAlias["single"], nil
}

func (c *ICloudClient) threadMessagesForAliases(ctx context.Context, session ICloudSession, folder mailFolder, threadID string, aliases map[string]string, afterByMailbox map[string]time.Time) (map[string][]ICloudSyncedMessage, error) {
	var out struct {
		MessageMetadataList []struct {
			UID       json.RawMessage `json:"uid"`
			Folder    string          `json:"folder"`
			MessageID string          `json:"messageId"`
			Subject   string          `json:"subject"`
			Preview   string          `json:"preview"`
			Date      json.RawMessage `json:"date"`
			From      json.RawMessage `json:"from"`
			To        json.RawMessage `json:"to"`
			CC        json.RawMessage `json:"cc"`
			BCC       json.RawMessage `json:"bcc"`
			Parts     []struct {
				PartID      string `json:"partId"`
				ContentType string `json:"contentType"`
				IsAttach    bool   `json:"isAttach"`
				FileName    string `json:"fileName"`
				Disposition string `json:"disposition"`
			} `json:"parts"`
		} `json:"messageMetadataList"`
	}
	body := map[string]any{
		"threadId":       threadID,
		"sessionHeaders": mailSessionHeaders(folder.Name, false),
	}
	if err := c.callMail(ctx, session, "/mailws2/v1/thread/get", body, "", &out); err != nil {
		return nil, err
	}
	messages := make(map[string][]ICloudSyncedMessage)
	for _, meta := range out.MessageMetadataList {
		uid := rawScalarString(meta.UID)
		if uid == "" {
			continue
		}
		receivedAt := firstNonZeroTime(parseMailTime(meta.Date), time.Now())
		folderName := firstNonEmpty(cleanMailFolder(meta.Folder), folder.Name)
		from := addressSummary(meta.From)
		recipients := mailMetadataRecipients(meta.To) + "\n" + mailMetadataRecipients(meta.CC) + "\n" + mailMetadataRecipients(meta.BCC)
		bodyText := meta.Subject + "\n" + meta.Preview
		partIDs := textPartIDs(meta.Parts)
		if len(partIDs) > 0 {
			detail, err := c.messageBody(ctx, session, folderName, uid, partIDs)
			if err != nil {
				return messages, err
			}
			if message, err := mail.ReadMessage(strings.NewReader(detail.LongHeader + "\r\n\r\n")); err == nil {
				recipients += "\n" + mailRecipientText(message.Header)
			}
			bodyText += "\n" + detail.Body
		}
		matchedMailboxIDs := matchingMailboxIDs(recipients, aliases)
		if len(matchedMailboxIDs) == 0 {
			continue
		}
		message := ICloudSyncedMessage{
			RemoteID:   "icloud:" + folderName + ":" + uid,
			UID:        uid,
			Subject:    meta.Subject,
			From:       from,
			Body:       normalizeMailBody(bodyText),
			LoginURL:   services.ExtractClaudeLoginURL(bodyText),
			ReceivedAt: receivedAt,
		}
		for _, mailboxID := range matchedMailboxIDs {
			after := afterByMailbox[mailboxID]
			if !after.IsZero() && receivedAt.Before(after) {
				continue
			}
			messages[mailboxID] = append(messages[mailboxID], message)
		}
	}
	return messages, nil
}

func matchingMailboxIDs(recipients string, aliases map[string]string) []string {
	if len(aliases) == 0 {
		return nil
	}
	var ids []string
	addresses := make(map[string]bool)
	for _, address := range strings.Split(recipients, "\n") {
		addresses[strings.ToLower(strings.TrimSpace(address))] = true
	}
	for mailboxID, alias := range aliases {
		if strings.TrimSpace(mailboxID) == "" || strings.TrimSpace(alias) == "" {
			continue
		}
		if addresses[strings.ToLower(strings.TrimSpace(alias))] {
			ids = append(ids, mailboxID)
		}
	}
	sort.Strings(ids)
	return ids
}

func mailMetadataRecipients(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	values, ok := value.([]any)
	if !ok {
		values = []any{value}
	}
	var out []string
	for _, value := range values {
		var text string
		switch v := value.(type) {
		case string:
			text = v
		case map[string]any:
			text, _ = v["email"].(string)
		}
		out = append(out, mailRecipientText(mail.Header{"To": []string{text}}))
	}
	return strings.Join(out, "\n")
}

func (c *ICloudClient) messageBody(ctx context.Context, session ICloudSession, folderName, uid string, partIDs []string) (mailMessageDetail, error) {
	var out struct {
		LongHeader string `json:"longHeader"`
		Parts      []struct {
			GUID    string `json:"guid"`
			Content string `json:"content"`
		} `json:"parts"`
	}
	body := map[string]any{
		"uid":            uid,
		"parts":          partIDs,
		"dontMarkAsRead": true,
		"sessionHeaders": mailSessionHeaders(folderName, false),
	}
	if err := c.callMail(ctx, session, "/mailws2/v1/message/get", body, "", &out); err != nil {
		return mailMessageDetail{}, err
	}
	var parts []string
	for _, part := range out.Parts {
		parts = append(parts, part.Content)
	}
	return mailMessageDetail{LongHeader: out.LongHeader, Body: strings.Join(parts, "\n")}, nil
}

func (c *ICloudClient) MoveRemoteMessagesToTrash(ctx context.Context, session ICloudSession, remoteIDs []string) (ICloudMailCleanupResult, error) {
	var result ICloudMailCleanupResult
	if strings.TrimSpace(session.DSID) == "" || len(session.Cookies) == 0 {
		return result, errCode("icloud_session_missing", "未保存 iCloud 登录态，请先协议登录", true)
	}
	folders, err := c.mailFolders(ctx, session)
	if err != nil {
		return result, err
	}
	trash, ok := trashMailFolder(folders)
	if !ok || strings.TrimSpace(trash.ID) == "" {
		return result, errCode("icloud_trash_not_found", "未找到 iCloud 废纸篓文件夹", true)
	}

	byFolder := make(map[string][]string)
	for _, remoteID := range remoteIDs {
		folderName, uid, ok := parseICloudRemoteID(remoteID)
		if !ok {
			result.Skipped++
			continue
		}
		if strings.EqualFold(folderName, trash.Name) {
			result.Skipped++
			continue
		}
		byFolder[folderName] = append(byFolder[folderName], uid)
	}
	for folderName, uids := range byFolder {
		folder, ok := findMailFolderByName(folders, folderName)
		if !ok || strings.TrimSpace(folder.ID) == "" {
			result.Skipped += len(uids)
			continue
		}
		ids, err := c.mailMessageIdentifiers(ctx, session, folder, uids)
		if err != nil {
			return result, err
		}
		for _, uid := range uniqueStrings(uids) {
			if strings.TrimSpace(ids[uid]) == "" {
				result.Skipped++
			}
		}
		moved, err := c.moveMailIdentifiersToTrash(ctx, session, mapValues(ids), trash.ID)
		result.MovedToTrash += moved
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func (c *ICloudClient) mailMessageIdentifiers(ctx context.Context, session ICloudSession, folder mailFolder, uids []string) (map[string]string, error) {
	out := make(map[string]string)
	for _, chunk := range chunkStrings(uniqueStrings(uids), 200) {
		var resp struct {
			DomainObjects []mailEmailObject `json:"domainObjects"`
		}
		body := map[string]any{
			"domain": "email",
			"predicate": map[string]any{
				"type":       "in",
				"expression": map[string]any{"property": "uid"},
				"value":      mailUIDValues(chunk),
				"and": []any{
					map[string]any{
						"type": "eq",
						"expression": map[string]any{
							"type":     "fieldOf",
							"property": "flags",
							"value":    "DELETED",
						},
						"value": false,
					},
					map[string]any{
						"type":       "eq",
						"expression": map[string]any{"property": "mboxRef"},
						"value":      folder.ID,
					},
				},
			},
			"properties": []string{"uid", "identifier", "mboxRef"},
		}
		if err := c.callMail(ctx, session, "/mailws2/v1/message/list", body, "", &resp); err != nil {
			return out, err
		}
		for _, item := range resp.DomainObjects {
			uid := rawScalarString(item.UID)
			identifier := strings.TrimSpace(item.Identifier)
			if uid == "" || identifier == "" {
				continue
			}
			out[uid] = identifier
		}
	}
	return out, nil
}

func (c *ICloudClient) moveMailIdentifiersToTrash(ctx context.Context, session ICloudSession, identifiers []string, trashFolderID string) (int, error) {
	identifiers = uniqueStrings(identifiers)
	if len(identifiers) == 0 {
		return 0, nil
	}
	moved := 0
	for _, chunk := range chunkStrings(identifiers, 200) {
		var resp mailSetResponse
		body := map[string]any{
			"domain": "email",
			"batchUpdate": []any{
				map[string]any{
					"ids": chunk,
					"patch": map[string]any{
						"flags":   map[string]any{"set": []string{"seen"}},
						"mboxRef": map[string]any{"replace": []string{trashFolderID}},
					},
				},
			},
		}
		if err := c.callMail(ctx, session, "/mailws2/v1/email/set", body, "", &resp); err != nil {
			return moved, err
		}
		if len(resp.NotUpdated) > 0 {
			return moved + len(resp.Updated), fmt.Errorf("icloud mail move notUpdated=%d", len(resp.NotUpdated))
		}
		if len(resp.Updated) > 0 {
			moved += len(resp.Updated)
		} else {
			moved += len(chunk)
		}
	}
	return moved, nil
}

func parseICloudRemoteID(remoteID string) (string, string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(remoteID), "icloud:")
	if !ok {
		return "", "", false
	}
	folderName, uid, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(folderName) == "" || strings.TrimSpace(uid) == "" {
		return "", "", false
	}
	return strings.TrimSpace(folderName), strings.TrimSpace(uid), true
}

func findMailFolderByName(folders []mailFolder, name string) (mailFolder, bool) {
	name = strings.TrimSpace(name)
	for _, folder := range folders {
		if strings.EqualFold(strings.TrimSpace(folder.Name), name) {
			return folder, true
		}
	}
	return mailFolder{}, false
}

func trashMailFolder(folders []mailFolder) (mailFolder, bool) {
	for _, folder := range folders {
		name := strings.ToLower(strings.TrimSpace(folder.Name))
		if name == "deleted messages" || name == "trash" || strings.Contains(name, "deleted") || strings.Contains(name, "trash") || strings.Contains(name, "废纸") {
			return folder, true
		}
	}
	return mailFolder{}, false
}

func mailUIDValues(uids []string) []any {
	out := make([]any, 0, len(uids))
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if n, err := strconv.Atoi(uid); err == nil {
			out = append(out, n)
			continue
		}
		out = append(out, uid)
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 {
		size = len(values)
	}
	var chunks [][]string
	for len(values) > 0 {
		n := size
		if len(values) < n {
			n = len(values)
		}
		chunks = append(chunks, values[:n])
		values = values[n:]
	}
	return chunks
}

func (c *ICloudClient) callMail(ctx context.Context, session ICloudSession, path string, body any, clientIntent string, result any) error {
	base, err := mailGatewayBaseURL(session)
	if err != nil {
		return err
	}
	u, err := c.endpointWithBase(session, base, path)
	if err != nil {
		return err
	}
	if clientIntent != "" {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		q.Set("clientIntent", clientIntent)
		parsed.RawQuery = q.Encode()
		u = parsed.String()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, reader)
	if err != nil {
		return err
	}
	if clientIntent != "" {
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	setICloudFetchHeaders(req, session, "*/*", req.Header.Get("Content-Type"))
	if cookie := cookieHeader(session.Cookies, u); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("icloud mail %s HTTP %d: %s", path, resp.StatusCode, appleDebugBody(data))
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return errCode("icloud_mail_bad_response", "iCloud 邮件返回无法解析", true)
		}
	}
	return nil
}

func (c *ICloudClient) endpoint(session ICloudSession, path string) (string, error) {
	return c.endpointWithBase(session, session.PremiumMailBaseURL, path)
}

func (c *ICloudClient) endpointWithBase(session ICloudSession, baseURL, path string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	if err != nil {
		return "", err
	}
	rel := &url.URL{Path: strings.TrimLeft(path, "/")}
	u := base.ResolveReference(rel)
	q := u.Query()
	q.Set("clientBuildNumber", firstNonEmpty(session.ClientBuildNumber, "2622Build20"))
	q.Set("clientMasteringNumber", firstNonEmpty(session.MasteringNumber, session.ClientBuildNumber, "2622Build20"))
	q.Set("clientId", firstNonEmpty(session.ClientID, "local-panel"))
	q.Set("dsid", session.DSID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func mailGatewayBaseURL(session ICloudSession) (string, error) {
	if strings.TrimSpace(session.MailGatewayBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(session.MailGatewayBaseURL), "/"), nil
	}
	if strings.TrimSpace(session.MailBaseURL) != "" {
		base := strings.TrimRight(strings.TrimSpace(session.MailBaseURL), "/")
		base = strings.Replace(base, "-mailws.", "-mccgateway.", 1)
		return base, nil
	}
	if strings.TrimSpace(session.PremiumMailBaseURL) != "" {
		base := strings.TrimRight(strings.TrimSpace(session.PremiumMailBaseURL), "/")
		base = strings.Replace(base, "-maildomainws.", "-mccgateway.", 1)
		return base, nil
	}
	return "", errCode("icloud_mail_missing", "未保存 iCloud 邮件服务地址，请重新保存登录态", true)
}

func mailSessionHeaders(folder string, reset bool) map[string]any {
	headers := map[string]any{
		"folder":       folder,
		"modseq":       nil,
		"threadmodseq": nil,
		"condstore":    1,
		"qresync":      1,
		"threadmode":   1,
	}
	if reset {
		headers["modseq"] = nil
		headers["threadmodseq"] = nil
	}
	return headers
}

func textPartIDs(parts []struct {
	PartID      string `json:"partId"`
	ContentType string `json:"contentType"`
	IsAttach    bool   `json:"isAttach"`
	FileName    string `json:"fileName"`
	Disposition string `json:"disposition"`
}) []string {
	var ids []string
	for _, part := range parts {
		if part.PartID == "" || part.IsAttach || part.FileName != "" {
			continue
		}
		contentType := strings.ToLower(part.ContentType)
		if strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "text/html") {
			ids = append(ids, strings.TrimSpace(part.PartID))
		}
	}
	return ids
}

func parseMailTime(raw json.RawMessage) time.Time {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		if n > 1_000_000_000 {
			return time.Unix(n, 0)
		}
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		n := int64(f)
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n)
		}
		if n > 1_000_000_000 {
			return time.Unix(n, 0)
		}
	}
	for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

func rawScalarString(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		return number.String()
	}
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

func addressSummary(raw json.RawMessage) string {
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return strings.Trim(strings.TrimSpace(string(raw)), `"`)
	}
	var out []string
	for _, value := range values {
		switch v := value.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			email, _ := v["email"].(string)
			name, _ := v["name"].(string)
			out = append(out, strings.TrimSpace(name+" <"+email+">"))
		}
	}
	return strings.Join(out, ", ")
}

func cleanMailFolder(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.Index(value, ":"); idx >= 0 && idx+1 < len(value) {
		return value[idx+1:]
	}
	return value
}

func containsFold(text, needle string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(strings.TrimSpace(needle)))
}

func normalizeMailBody(value string) string {
	value = html.UnescapeString(value)
	value = htmlTagRegex.ReplaceAllString(value, " ")
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func cookieHeader(cookies []SessionCookie, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	nowUnix := float64(time.Now().Unix())
	type cookiePair struct {
		name  string
		value string
		order int
		index int
	}
	var pairs []cookiePair
	for i, cookie := range cookies {
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		if cookie.Expires > 0 && cookie.Expires < nowUnix {
			continue
		}
		if !cookieDomainMatch(host, cookie.Domain) {
			continue
		}
		if cookie.Path != "" && !strings.HasPrefix(path, cookie.Path) {
			continue
		}
		pairs = append(pairs, cookiePair{
			name:  cookie.Name,
			value: cookie.Value,
			order: preferredICloudCookieOrder(cookie.Name),
			index: i,
		})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].order != pairs[j].order {
			return pairs[i].order < pairs[j].order
		}
		return pairs[i].index < pairs[j].index
	})
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, pair.name+"="+pair.value)
	}
	return strings.Join(out, "; ")
}

func mergeSessionCookies(cookies *[]SessionCookie, requestURL *url.URL, setCookies []*http.Cookie) {
	if cookies == nil {
		return
	}
	for _, c := range setCookies {
		domain := strings.TrimSpace(c.Domain)
		if domain == "" && requestURL != nil {
			domain = requestURL.Hostname()
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		next := SessionCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   domain,
			Path:     path,
			Secure:   c.Secure,
			HTTPOnly: c.HttpOnly,
		}
		if !c.Expires.IsZero() {
			next.Expires = float64(c.Expires.Unix())
		}
		if c.MaxAge > 0 {
			next.Expires = float64(time.Now().Unix()) + float64(c.MaxAge)
		}
		if c.MaxAge < 0 || (next.Expires > 0 && next.Expires <= float64(time.Now().Unix())) {
			next.Value = ""
		}
		replaced := false
		for i, old := range *cookies {
			if old.Name == next.Name && strings.EqualFold(strings.TrimPrefix(old.Domain, "."), strings.TrimPrefix(next.Domain, ".")) && old.Path == next.Path {
				if next.Value == "" {
					*cookies = append((*cookies)[:i], (*cookies)[i+1:]...)
				} else {
					(*cookies)[i] = next
				}
				replaced = true
				break
			}
		}
		if !replaced && next.Value != "" {
			*cookies = append(*cookies, next)
		}
	}
}

func preferredICloudCookieOrder(name string) int {
	for i, candidate := range preferredICloudCookies {
		if candidate == "X_APPLE_WEB_KB-" && strings.HasPrefix(name, candidate) {
			return i
		}
		if candidate != "X_APPLE_WEB_KB-" && name == candidate {
			return i
		}
	}
	return len(preferredICloudCookies) + 100
}

func cookieDomainMatch(host, domain string) bool {
	domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func trimForError(data []byte) string {
	text := strings.TrimSpace(string(data))
	if len(text) > 240 {
		return text[:240] + "..."
	}
	return text
}

func appleDebugBody(data []byte) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err == nil {
		redacted := redactAppleDebugJSON(value)
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(redacted); err == nil {
			return trimForError(bytes.TrimSpace(buf.Bytes()))
		}
	}
	return fmt.Sprintf("<非 JSON 响应，%d 字节>", len(trimmed))
}

func redactAppleDebugJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if appleDebugSecretKey(key) {
				out[key] = "<redacted>"
				continue
			}
			out[key] = redactAppleDebugJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactAppleDebugJSON(item)
		}
		return out
	default:
		return value
	}
}

func appleDebugSecretKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"apikey", "api_key", "token", "secret", "password", "scnt", "session", "cookie", "authorization", "email", "accountname", "appleid", "dsid"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func setICloudFetchHeaders(req *http.Request, session ICloudSession, accept, contentType string) {
	if strings.TrimSpace(accept) == "" {
		accept = "*/*"
	}
	req.Header.Set("Accept", accept)
	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("Content-Type", contentType)
	}
	origin := iCloudOrigin(session)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="143", "Chromium";v="143", "Not A(Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
}

func iCloudOrigin(session ICloudSession) string {
	host := strings.ToLower(session.Host)
	if strings.Contains(host, "icloud.com.cn") {
		return "https://www.icloud.com.cn"
	}
	return "https://www.icloud.com"
}
