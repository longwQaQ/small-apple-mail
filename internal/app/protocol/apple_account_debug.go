package protocol

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func updateAppleAccountLoginStateFromHeaders(loginState *LoginState, header http.Header) {
	if loginState == nil {
		return
	}
	if scnt := strings.TrimSpace(header.Get("scnt")); scnt != "" {
		loginState.Scnt = scnt
	}
	if sessionID := strings.TrimSpace(header.Get("X-Apple-ID-Session-Id")); sessionID != "" {
		loginState.SessionID = sessionID
	}
	if token := strings.TrimSpace(firstNonEmpty(header.Get("X-Apple-I-DA-Token"), header.Get("X-Apple-I-Cont-X-Apple-I-DA-Token"))); token != "" {
		loginState.DataAccessToken = token
	}
	loginState.SavedAt = time.Now()
}

func appleAccountJSLogEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("small-apple-mail-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	)
}

func appleAccountFDClientInfo(userAgent string) string {
	info := map[string]string{
		"U": firstNonEmpty(userAgent, appleAccountManageUserAgent),
		"L": appleAccountManageLanguage,
		"Z": appleAccountManageGMTOffset,
		"V": "1.1",
		"F": appleAccountCompressedFingerprint(time.Now()),
	}
	data, _ := json.Marshal(info)
	return string(data)
}

func appleAccountCompressedFingerprint(now time.Time) string {
	raw := appleAccountFingerprintPayload(now.In(time.FixedZone("apple-account", appleAccountManageTZOffset)))
	replaced := raw
	for idx, token := range appleAccountFingerprintDictionary {
		replaced = strings.ReplaceAll(replaced, token, string(rune(idx+1)))
	}
	encoded, ok := appleAccountFingerprintHuffman(replaced)
	if !ok {
		return raw
	}
	checksum := 65535
	for _, b := range []byte(raw) {
		checksum = ((checksum >> 8) | (checksum << 8)) & 0xffff
		checksum ^= int(b) & 0xff
		checksum ^= (checksum & 0xff) >> 4
		checksum ^= (checksum << 12) & 0xffff
		checksum ^= ((checksum & 0xff) << 5) & 0xffff
	}
	return encoded +
		string(appleAccountFingerprintAlphabet[(checksum>>12)&63]) +
		string(appleAccountFingerprintAlphabet[(checksum>>6)&63]) +
		string(appleAccountFingerprintAlphabet[checksum&63])
}

func appleAccountFingerprintPayload(now time.Time) string {
	values := []string{
		"TF1", "020",
	}
	for i := 0; i < 39; i++ {
		values = append(values, "")
	}
	values = append(values,
		"true",
		"true",
		strconv.FormatInt(now.UnixMilli(), 10),
		"-6",
		"6/7/2005, 9:33:44 PM",
		"", "", "", "", "", "",
		strconv.FormatInt(now.UnixMilli(), 10),
		"0",
		appleAccountUSLocaleString(now),
	)
	for i := 0; i < 34; i++ {
		values = append(values, "")
	}
	values = append(values, "5.6.1-0", "")

	var b strings.Builder
	for _, value := range values {
		b.WriteString(appleAccountJSEscape(value))
		b.WriteByte(';')
	}
	return b.String()
}

func appleAccountUSLocaleString(t time.Time) string {
	hour := t.Hour()
	ampm := "AM"
	if hour >= 12 {
		ampm = "PM"
	}
	hour12 := hour % 12
	if hour12 == 0 {
		hour12 = 12
	}
	return fmt.Sprintf("%d/%d/%d, %d:%02d:%02d %s", int(t.Month()), t.Day(), t.Year(), hour12, t.Minute(), t.Second(), ampm)
}

func appleAccountJSEscape(value string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '@' || r == '*' || r == '_' || r == '+' || r == '-' || r == '.' || r == '/' {
			b.WriteRune(r)
			continue
		}
		if r <= 0xff {
			b.WriteByte('%')
			b.WriteByte(hex[(r>>4)&0xf])
			b.WriteByte(hex[r&0xf])
			continue
		}
		b.WriteString("%u")
		b.WriteByte(hex[(r>>12)&0xf])
		b.WriteByte(hex[(r>>8)&0xf])
		b.WriteByte(hex[(r>>4)&0xf])
		b.WriteByte(hex[r&0xf])
	}
	return b.String()
}

func appleAccountFingerprintHuffman(value string) (string, bool) {
	var b strings.Builder
	bitBuffer := 0
	bitCount := 0
	push := func(width, code int) {
		bitBuffer = (bitBuffer << width) | code
		bitCount += width
		for bitCount >= 6 {
			idx := (bitBuffer >> (bitCount - 6)) & 63
			b.WriteByte(appleAccountFingerprintAlphabet[idx])
			bitCount -= 6
			bitBuffer ^= idx << bitCount
		}
	}
	push(6, (len(value)&7)<<3)
	push(6, (len(value)&56)|1)
	for _, r := range value {
		code, ok := appleAccountFingerprintCodes[int(r)]
		if !ok {
			return "", false
		}
		push(code.width, code.value)
	}
	code := appleAccountFingerprintCodes[0]
	push(code.width, code.value)
	if bitCount > 0 {
		push(6-bitCount, 0)
	}
	return b.String(), true
}

func appleAccountAPIError(status int, data []byte, stage string) error {
	detail := appleAccountErrorDetail(status, data, stage)
	msg := strings.TrimSpace(appleDebugBody(data))
	if msg == "" {
		msg = "空响应"
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "limit") || strings.Contains(lower, "too many") || strings.Contains(lower, "rate") {
		return errCode("apple_account_hme_limit", "Apple Account 已达到当前隐私邮箱创建上限，请稍后再试；"+detail, true)
	}
	if status == appleAccountHTTPStatusSessionTimeout || ((status == http.StatusUnauthorized || status == http.StatusForbidden) && appleAccountBodyLooksAuthExpired(lower)) {
		return errCode("apple_account_auth_failed", "Apple Account 管理态已失效，请重新协议登录；"+detail, true)
	}
	return errCode("apple_account_api_failed", "Apple Account 接口失败；"+detail, true)
}

func appleAccountBodyLooksAuthExpired(lower string) bool {
	lower = strings.ToLower(strings.TrimSpace(lower))
	if lower == "" {
		return false
	}
	authTokens := []string{
		"authentication_failed",
		"authentication failed",
		"auth_failed",
		"auth failed",
		"gsa_invalid_session",
		"invalid session",
		"scnt_expired",
		"session expired",
		"session has expired",
	}
	for _, token := range authTokens {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return strings.Contains(lower, "authentication") && (strings.Contains(lower, "failed") || strings.Contains(lower, "expired"))
}

func appleAccountErrorDetail(status int, data []byte, stage string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "未知阶段"
	}
	body := strings.TrimSpace(appleDebugBody(data))
	if body == "" {
		body = "空响应"
	}
	return fmt.Sprintf("阶段：%s；HTTP %d；原始返回：%s", stage, status, body)
}

func appleAccountResponseDetail(stage string, data []byte) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "未知阶段"
	}
	body := strings.TrimSpace(appleDebugBody(data))
	if body == "" {
		body = "空响应"
	}
	return fmt.Sprintf("阶段：%s；原始返回：%s", stage, body)
}

func appleAccountRawResponseDetail(stage string, raw appleAccountRawResponse) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "未知阶段"
	}
	body := strings.TrimSpace(appleDebugBody(raw.Body))
	if body == "" {
		body = "空响应"
	}
	if raw.StatusCode > 0 {
		return fmt.Sprintf("阶段：%s；HTTP %d；原始返回：%s", stage, raw.StatusCode, body)
	}
	return fmt.Sprintf("阶段：%s；原始返回：%s", stage, body)
}

func appleAccountRequestStage(method, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	switch {
	case path == "/account/manage/gs/ws/token":
		return "刷新管理 token"
	case path == "/account/manage":
		return "读取管理入口"
	case path == "/account/manage/section/privacy":
		return "打开隐私页面"
	case path == "/bootstrap/portal":
		return "预热门户"
	case method == http.MethodPost && path == "/v2/jslogs":
		return "备用创建通道保活"
	case method == http.MethodGet && path == "/account/manage/forwardemail":
		return "读取转发邮箱"
	case method == http.MethodPost && path == "/account/manage/email/private/add":
		return "生成候选隐私邮箱"
	case method == http.MethodPut && path == "/account/manage/email/private/add/complete":
		return "确认创建隐私邮箱"
	case method == http.MethodGet && strings.HasPrefix(path, "/account/manage/email/private/") && strings.HasSuffix(path, ".em"):
		return "确认隐私邮箱详情"
	default:
		return strings.TrimSpace(method + " " + path)
	}
}

func isCodedError(err error, code string) bool {
	var coded codedError
	return errors.As(err, &coded) && coded.code == code
}
