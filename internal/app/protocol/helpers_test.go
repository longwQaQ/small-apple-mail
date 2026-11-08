package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAppleEndpointPhoneAndChallengeHelpers(t *testing.T) {
	if normalizeAppleTwoFactorMethod("sms") != appleTwoFactorMethodPhone || normalizeAppleTwoFactorMethod("") != appleTwoFactorMethodTrustedDevice {
		t.Fatal("2FA 方式标准化错误")
	}
	if appleAuthEndpointsForHost("www.icloud.com.cn").Host != "www.icloud.com.cn" || appleAuthEndpointsForHost("www.icloud.com").Host != "www.icloud.com" {
		t.Fatal("Apple 端点选择错误")
	}
	if appleDomainToHost("https://www.icloud.com.cn/") != "www.icloud.com.cn" || appleHostForAccountCountry("CHN") != "www.icloud.com.cn" || appleHostForAccountCountry("USA") != "www.icloud.com" {
		t.Fatal("Apple 区域解析错误")
	}
	redirect, ok := parseAppleDomainRedirect(http.StatusFound, []byte(`{"domainToUse":"iCloud.com.cn"}`))
	if !ok || redirect.Host != "www.icloud.com.cn" || !strings.Contains(redirect.Error(), "icloud.com.cn") {
		t.Fatalf("redirect=%+v ok=%v", redirect, ok)
	}
	if _, ok = parseAppleDomainRedirect(http.StatusOK, nil); ok {
		t.Fatal("非重定向响应不应被识别")
	}

	fallback := appleAccountFallbackPhoneNumber(nil)
	phone, err := appleAccountPhoneNumberPayload(fallback, false)
	if err != nil || phone["id"] == nil || phone["nonFTEU"] != nil {
		t.Fatalf("phone=%+v err=%v", phone, err)
	}
	if _, err = appleAccountPhoneNumberPayload(json.RawMessage(`{"number":"1"}`), true); err == nil {
		t.Fatal("缺少 phone id 应失败")
	}
	session := &appleAuthSession{}
	session.rememberTwoFactorPhoneNumber([]byte(`<script id="app_config">{"trustedPhoneNumbers":[{"id":7,"nonFTEU":true}]}</script>`))
	if !bytes.Contains(session.TwoFactorPhone, []byte(`"id":7`)) {
		t.Fatalf("phone=%s", session.TwoFactorPhone)
	}
	if extracted := extractAppleAppConfigJSON([]byte(`<script id='app_config'>{"ok":true}</script>`)); !bytes.Contains(extracted, []byte(`"ok":true`)) {
		t.Fatalf("extracted=%s", extracted)
	}

	hashcash, err := generateAppleHashcash(4, "challenge", time.Unix(0, 0))
	if err != nil || hashcash == "" {
		t.Fatalf("hashcash=%q err=%v", hashcash, err)
	}
	if leadingZeroBits([]byte{0, 0x10}) != 11 {
		t.Fatal("前导零位计算错误")
	}
	if _, err = generateAppleHashcash(0, "", time.Now()); err == nil {
		t.Fatal("空挑战应失败")
	}
}

func TestApplePendingSessionAndHeaders(t *testing.T) {
	store := newAppleAuthPendingStore()
	pending, err := store.put(&appleAuthSession{AppleID: "user@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded, ok := store.get("  " + pending.ID + "  "); !ok || loaded.Session.AppleID != "user@example.com" {
		t.Fatalf("pending=%+v ok=%v", loaded, ok)
	}
	store.delete(pending.ID)
	if _, ok := store.get(pending.ID); ok {
		t.Fatal("待验证会话未删除")
	}
	store.items["expired"] = appleAuthPending{ExpiresAt: time.Now().Add(-time.Second)}
	store.cleanupLocked(time.Now())
	if len(store.items) != 0 {
		t.Fatal("过期待验证会话未清理")
	}

	s := &appleAuthSession{Endpoints: appleAuthEndpointsForHost("www.icloud.com"), ClientID: "client", FrameID: "frame", UserAgent: "ua"}
	requestURL, _ := url.Parse("https://idmsa.apple.com/appleauth/auth")
	response := &http.Response{Request: &http.Request{URL: requestURL}, Header: make(http.Header)}
	response.Header.Set("X-Apple-ID-Account-Country", "CHN")
	response.Header.Set("X-Apple-ID-Session-Id", "session")
	response.Header.Set("X-Apple-Session-Token", "token")
	response.Header.Set("Scnt", "scnt")
	response.Header.Set("X-Apple-HC-Bits", "5")
	response.Header.Set("X-Apple-HC-Challenge", "challenge")
	s.extract(response)
	if s.AccountCountry != "CHN" || s.SessionID != "session" || s.HCBits != 5 {
		t.Fatalf("session=%+v", s)
	}
	s.rememberCompleteHashcashChallenge()
	bits, challenge := s.completeHashcashChallenge()
	if bits != 5 || challenge != "challenge" {
		t.Fatalf("bits=%d challenge=%q", bits, challenge)
	}
	if !s.switchHost("www.icloud.com.cn") || s.switchHost("www.icloud.com.cn") {
		t.Fatal("主机切换判断错误")
	}
	if _, ok := s.redirectForAccountCountry(); ok {
		t.Fatal("当前区域相同时不应重定向")
	}
	manage := &appleAuthSession{Endpoints: appleAccountManageAuthEndpoints(), ClientID: "client"}
	if !manage.isAppleAccountManage() || manage.twoFactorHeaders()["X-Apple-App-Id"] != "client" {
		t.Fatal("Apple Account 管理请求头错误")
	}
}

func TestSRPAndGeneralProtocolHelpers(t *testing.T) {
	client, err := newSRPClient()
	if err != nil || len(client.ABytes()) != srpNLenBytes {
		t.Fatalf("client=%v err=%v", client, err)
	}
	derived, err := deriveAppleSRPPassword("password", []byte("salt"), 2, "s2k")
	if err != nil || len(derived) != 32 {
		t.Fatalf("derived=%d err=%v", len(derived), err)
	}
	if _, err = deriveAppleSRPPassword("password", nil, 1, "unknown"); err == nil {
		t.Fatal("未知 SRP 协议应失败")
	}
	if err = client.processChallenge([]byte("user"), derived, []byte("salt"), srpPad(big.NewInt(5))); err != nil || len(client.M1) != 32 || len(client.M2) != 32 {
		t.Fatalf("M1=%d M2=%d err=%v", len(client.M1), len(client.M2), err)
	}
	if err = client.processChallenge(nil, derived, nil, nil); err == nil {
		t.Fatal("无效服务端 B 应失败")
	}
	if token, err := randomToken(8); err != nil || token == "" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if id, err := randomUUID(); err != nil || strings.Count(id, "-") != 4 {
		t.Fatalf("uuid=%q err=%v", id, err)
	}
	if maskSecret("abcdef", 2) != "********ef" || formatTime(time.Time{}) != "" || firstNonEmpty("", " value ") != "value" {
		t.Fatal("通用格式化辅助函数错误")
	}
	if got := normalizeAccountIDSelection("a,b", []string{"a", "c"}); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("selection=%v", got)
	}
}

func TestAppleStateErrorsAndRedaction(t *testing.T) {
	now := time.Now()
	state := LoginState{Kind: LoginStateAppleAccount, Scnt: "scnt", APIKey: "api-key", LastCheckedAt: now, LastCheckOK: true, ManageExpiresAt: now.Add(time.Hour)}
	session := ICloudSession{AccountID: "acc", LoginStates: []LoginState{state}}
	if !appleAccountManageReady(session) || !appleAccountManageRecentlyOK(state, now) || appleAccountManageNeedsCreateRefresh(state, now) {
		t.Fatal("Apple Account 登录态判断错误")
	}
	next := state
	next.Scnt = "next"
	session = withAppleAccountLoginState(session, next)
	if session.LoginStates[0].Scnt != "next" || appleAccountOperationKey(session, next) == "" {
		t.Fatal("Apple Account 登录态替换错误")
	}
	markAppleAccountManageOK(&next)
	markAppleAccountManageTokenTTL(&next, 10, now)
	if !next.LastCheckOK || next.ManageExpiresAt.Before(now.Add(9*time.Minute)) {
		t.Fatal("Apple Account 登录态时间更新错误")
	}

	secret := "TOP-SECRET-PASSWORD"
	payload := []byte(`{"password":"` + secret + `","token":"TOKEN-SECRET","message":"authentication failed"}`)
	err := appleAccountAPIError(http.StatusUnauthorized, payload, "测试阶段")
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "TOKEN-SECRET") || !isCodedError(err, "apple_account_auth_failed") {
		t.Fatalf("错误脱敏失败: %v", err)
	}
	if !appleAccountBodyLooksAuthExpired("session has expired") || appleAccountBodyLooksAuthExpired("temporary outage") {
		t.Fatal("认证失效识别错误")
	}
	if appleAccountRequestStage(http.MethodGet, "/account/manage/forwardemail") != "读取转发邮箱" {
		t.Fatal("请求阶段映射错误")
	}
	if appleDebugFingerprint(secret) == secret || appleDebugFingerprint("") != "-" {
		t.Fatal("调试指纹错误")
	}
	if !isAppleTransientNetworkError(io.EOF) || isAppleTransientNetworkError(errors.New("business error")) {
		t.Fatal("瞬时网络错误识别错误")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(retryAppleTransient(cancelled, func() error { return io.EOF }), context.Canceled) {
		t.Fatal("重试未响应 Context 取消")
	}
}

func TestICloudMailCookieAndParsingHelpers(t *testing.T) {
	if firstNonEmptyRaw(nil, json.RawMessage(`null`), json.RawMessage(`"value"`)) == nil {
		t.Fatal("未选择首个有效 JSON 值")
	}
	if got := parseICloudRemoteTime(json.RawMessage(`"2026-07-19T01:02:03Z"`)); got.IsZero() || got.Location() != time.UTC {
		t.Fatalf("time=%v", got)
	}
	if got := parseICloudRemoteTime(json.RawMessage(`1760000000000`)); got.IsZero() {
		t.Fatal("毫秒时间戳解析失败")
	}
	if !isICloudHMELimitMessage("reached the limit of addresses") || isICloudHMELimitMessage("ok") || !isCodedError(iCloudAPIError("try again later: limit"), "icloud_hme_limit") {
		t.Fatal("iCloud 限额错误识别失败")
	}
	now := time.Now()
	if got := mailboxSyncAfter(Mailbox{LastSyncAt: now.Add(-time.Minute)}, time.Time{}, now); got.IsZero() || !looksLikeVerificationText("Your code is 123456", "") {
		t.Fatalf("syncAfter=%v", got)
	}
	if !shouldSkipSyncedThread(mailThread{ReceivedAt: now.Add(-time.Hour)}, now) || shouldSkipSyncedThread(mailThread{}, now) {
		t.Fatal("线程游标判断错误")
	}
	folders := preferredMailFolders([]mailFolder{{Name: "Sent Messages", MessageCount: 2}, {Name: "INBOX", MessageCount: 1}, {Name: "Archive", MessageCount: 1}})
	if len(folders) != 2 || folders[0].Name != "INBOX" {
		t.Fatalf("folders=%+v", folders)
	}
	if folder, uid, ok := parseICloudRemoteID("icloud:INBOX:123"); !ok || folder != "INBOX" || uid != "123" {
		t.Fatalf("folder=%q uid=%q ok=%v", folder, uid, ok)
	}
	if _, _, ok := parseICloudRemoteID("remote:bad"); ok {
		t.Fatal("无效远端 ID 不应通过")
	}
	if _, ok := findMailFolderByName(folders, "inbox"); !ok {
		t.Fatal("未找到邮箱目录")
	}
	if _, ok := trashMailFolder([]mailFolder{{Name: "Deleted Messages"}}); !ok {
		t.Fatal("未找到废纸篓")
	}
	if got := mailUIDValues([]string{"1", "abc", ""}); len(got) != 2 {
		t.Fatalf("uids=%v", got)
	}
	if got := uniqueStrings([]string{" a ", "a", "b"}); len(got) != 2 {
		t.Fatalf("unique=%v", got)
	}
	if got := mapValues(map[string]string{"b": "two", "a": "one", "empty": ""}); len(got) != 2 || got[0] != "one" {
		t.Fatalf("values=%v", got)
	}
	if got := chunkStrings([]string{"a", "b", "c"}, 2); len(got) != 2 || len(got[0]) != 2 {
		t.Fatalf("chunks=%v", got)
	}

	client := NewICloudClient()
	session := ICloudSession{DSID: "123", ClientID: "client", ClientBuildNumber: "build", Host: "www.icloud.com.cn", PremiumMailBaseURL: "https://p-maildomainws.icloud.com"}
	endpoint, err := client.endpoint(session, "/mailws2/v1/test")
	if err != nil || !strings.Contains(endpoint, "dsid=123") || iCloudOrigin(session) != "https://www.icloud.com.cn" {
		t.Fatalf("endpoint=%q err=%v", endpoint, err)
	}
	if gateway, err := mailGatewayBaseURL(session); err != nil || !strings.Contains(gateway, "mccgateway") {
		t.Fatalf("gateway=%q err=%v", gateway, err)
	}
	request := &http.Request{Header: make(http.Header)}
	setICloudFetchHeaders(request, session, "", "application/json")
	if request.Header.Get("Origin") != "https://www.icloud.com.cn" || request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers=%v", request.Header)
	}

	cookies := []SessionCookie{
		{Name: "other", Value: "2", Domain: ".icloud.com", Path: "/"},
		{Name: "X-APPLE-UNIQUE-CLIENT-ID", Value: "1", Domain: ".icloud.com", Path: "/"},
		{Name: "expired", Value: "x", Domain: ".icloud.com", Path: "/", Expires: float64(time.Now().Add(-time.Hour).Unix())},
	}
	header := cookieHeader(cookies, "https://www.icloud.com/mail")
	if header != "X-APPLE-UNIQUE-CLIENT-ID=1; other=2" || !cookieDomainMatch("www.icloud.com", ".icloud.com") {
		t.Fatalf("cookie=%q", header)
	}
	u, _ := url.Parse("https://www.icloud.com/")
	mergeSessionCookies(&cookies, u, []*http.Cookie{{Name: "other", Value: "3", Path: "/"}, {Name: "new", Value: "4"}})
	if !strings.Contains(cookieHeader(cookies, "https://www.icloud.com/"), "other=3") {
		t.Fatalf("cookies=%+v", cookies)
	}

	if got := parseMailTime(json.RawMessage(`"2026-07-19T01:02:03Z"`)); got.IsZero() || rawScalarString(json.RawMessage(`123`)) != "123" {
		t.Fatal("邮件标量解析失败")
	}
	if got := addressSummary(json.RawMessage(`[{"name":"User","email":"u@example.com"}]`)); !strings.Contains(got, "u@example.com") {
		t.Fatalf("address=%q", got)
	}
	if cleanMailFolder("prefix:INBOX") != "INBOX" || normalizeMailBody("<b>Hello&nbsp;world</b>") != "Hello world" {
		t.Fatal("邮件正文标准化失败")
	}
	parts := []struct {
		PartID      string `json:"partId"`
		ContentType string `json:"contentType"`
		IsAttach    bool   `json:"isAttach"`
		FileName    string `json:"fileName"`
		Disposition string `json:"disposition"`
	}{{PartID: "1", ContentType: "text/plain"}, {PartID: "2", ContentType: "image/png", IsAttach: true}}
	if got := textPartIDs(parts); len(got) != 1 || got[0] != "1" {
		t.Fatalf("parts=%v", got)
	}
}

func TestICloudRemoteMailboxSnapshotValidation(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"字段缺失":     nil,
		"字段为 null": json.RawMessage(`null`),
		"字段不是数组":   json.RawMessage(`{}`),
		"空邮箱地址": json.RawMessage(`[{
			"anonymousId":"remote-one",
			"hme":""
		}]`),
		"重复邮箱": json.RawMessage(`[
			{"anonymousId":"remote-one","hme":"same@icloud.com"},
			{"anonymousId":"remote-two","hme":"SAME@icloud.com"}
		]`),
		"重复远端标识": json.RawMessage(`[
			{"anonymousId":"remote-one","hme":"one@icloud.com"},
			{"anonymousId":"remote-one","hme":"two@icloud.com"}
		]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseICloudRemoteMailboxSnapshot(raw); err == nil || !isCodedError(err, "icloud_hme_snapshot_invalid") {
				t.Fatalf("err=%v", err)
			}
		})
	}

	empty, err := parseICloudRemoteMailboxSnapshot(json.RawMessage(`[]`))
	if err != nil || len(empty) != 0 {
		t.Fatalf("合法空快照应通过: rows=%v err=%v", empty, err)
	}
	rows, err := parseICloudRemoteMailboxSnapshot(json.RawMessage(`[
		{"anonymousId":" remote-one ","hme":" One@iCloud.com ","label":" Label ","isActive":true,"createTimestamp":1760000000000}
	]`))
	if err != nil || len(rows) != 1 || rows[0].AnonymousID != "remote-one" || rows[0].Email != "one@icloud.com" || rows[0].Label != "Label" || !rows[0].IsActive || rows[0].CreatedAt.IsZero() {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestIMAPAndFingerprintHelpers(t *testing.T) {
	if LoginStateICloudIMAP != "imap" {
		t.Fatalf("IMAP 连接类型必须与 API 和领域模型一致，got %q", LoginStateICloudIMAP)
	}
	if err := CheckConnection(context.Background(), "imap", `{"email":"","app_password":""}`); !isCodedError(err, "imap_email_missing") {
		t.Fatalf("imap 连接类型没有进入 IMAP 检测分支，err=%v", err)
	}
	state, err := normalizeICloudIMAPState(LoginState{IMAPEmail: " User@iCloud.com ", IMAPAppPassword: " app-pass "})
	if err != nil || state.IMAPHost != defaultICloudIMAPHost || state.IMAPPort != defaultICloudIMAPPort || state.IMAPUsername != "user@icloud.com" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err = normalizeICloudIMAPState(LoginState{}); err == nil {
		t.Fatal("缺少 IMAP 凭据应失败")
	}
	if size, ok := imapLiteralSize("* 1 FETCH {123}"); !ok || size != 123 {
		t.Fatalf("size=%d ok=%v", size, ok)
	}
	search := imapSearchUIDs([]string{"* SEARCH 3 1 2", "ignored"})
	fetch := imapFetchUIDs([]string{"* 1 FETCH (UID 9 FLAGS ())"})
	sortInts(search)
	if len(search) != 3 || search[0] != 1 || len(fetch) != 1 || fetch[0] != 9 || imapUIDSet(search) != "1,2,3" {
		t.Fatalf("search=%v fetch=%v", search, fetch)
	}
	if got := lastIntValues(search, 2); len(got) != 2 || got[0] != 2 {
		t.Fatalf("last=%v", got)
	}
	if got := chunkInts(search, 2); len(got) != 2 {
		t.Fatalf("chunks=%v", got)
	}
	header := mail.Header{"Subject": []string{"=?UTF-8?B?5rWL6K+V?="}}
	if got := decodeMIMEHeader(header.Get("Subject")); got != "测试" {
		t.Fatalf("header=%q", got)
	}
	body := decodeICloudIMAPBody(mail.Header{"Content-Type": []string{"text/plain"}, "Content-Transfer-Encoding": []string{"base64"}}, []byte("SGVsbG8="))
	if strings.TrimSpace(body) != "Hello" {
		t.Fatalf("body=%q", body)
	}
	messages := []ICloudSyncedMessage{{RemoteID: "old", ReceivedAt: time.Now().Add(-time.Hour)}, {RemoteID: "new", ReceivedAt: time.Now()}}
	sortMessagesByReceivedAt(messages)
	if messages[0].RemoteID != "new" {
		t.Fatalf("messages=%+v", messages)
	}

	now := time.Date(2026, 7, 19, 13, 2, 3, 0, time.UTC)
	if !strings.Contains(appleAccountUSLocaleString(now), "PM") || !strings.Contains(appleAccountJSEscape("中 文"), "%u") {
		t.Fatal("Apple 浏览器指纹格式错误")
	}
	payload := appleAccountFingerprintPayload(now)
	if payload == "" || appleAccountCompressedFingerprint(now) == "" || appleAccountFDClientInfo("ua") == "" || appleAccountJSLogEventID() == "" {
		t.Fatal("Apple 浏览器指纹生成失败")
	}
}
