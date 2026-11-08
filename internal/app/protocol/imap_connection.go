package protocol

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultICloudIMAPHost = "imap.mail.me.com"
	defaultICloudIMAPPort = 993
	imapIdleReadTimeout   = 25 * time.Minute
)

var iCloudIMAPDNSServers = []string{"1.1.1.1:53", "8.8.8.8:53", "127.0.0.53:53"}

var iCloudIMAPFallbackIPv4 = []net.IP{
	net.IPv4(17, 57, 152, 32),
	net.IPv4(17, 57, 152, 35),
	net.IPv4(17, 57, 155, 39),
	net.IPv4(17, 42, 251, 69),
	net.IPv4(17, 42, 251, 72),
	net.IPv4(17, 56, 9, 32),
	net.IPv4(17, 156, 192, 7),
}

type IMAPCursor struct {
	UIDValidity string
	LastUID     string
}

type IMAPSyncResult struct {
	Cursor            IMAPCursor
	HasMore           bool
	MessagesByMailbox map[string][]ICloudSyncedMessage
	LastUID           string
}

type iCloudIMAPSyncResult = IMAPSyncResult

func newICloudIMAPDialer(serverName string) tls.Dialer {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	return tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout:  15 * time.Second,
			Resolver: newICloudIMAPResolver(iCloudIMAPDNSServers),
		},
		Config: &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		},
	}
}

func dialICloudIMAPTLS(ctx context.Context, serverName string, port int) (net.Conn, error) {
	serverName = firstNonEmpty(strings.TrimSpace(serverName), defaultICloudIMAPHost)
	if port <= 0 {
		port = defaultICloudIMAPPort
	}
	var lastErr error
	if strings.EqualFold(strings.TrimSuffix(serverName, "."), defaultICloudIMAPHost) {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, iCloudIMAPFallbackIPv4); err == nil {
			return conn, nil
		} else {
			lastErr = err
		}
	}
	ips, lookupErr := lookupICloudIMAPIPv4(ctx, serverName)
	if len(appendUniqueIPv4(nil, ips...)) > 0 {
		if conn, err := dialICloudIMAPTLSIPs(ctx, serverName, port, ips); err == nil {
			return conn, nil
		} else if err != nil {
			lastErr = err
		}
	}
	address := net.JoinHostPort(serverName, strconv.Itoa(port))
	dialer := newICloudIMAPDialer(serverName)
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	if err == nil {
		return conn, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("IMAP IPv4 直连失败：%v；域名拨号失败：%w", lastErr, err)
	}
	if lookupErr != nil {
		return nil, fmt.Errorf("IMAP DNS-over-TCP 解析失败：%v；域名拨号失败：%w", lookupErr, err)
	}
	return nil, err
}

func dialICloudIMAPTLSIPs(ctx context.Context, serverName string, port int, ips []net.IP) (net.Conn, error) {
	ips = appendUniqueIPv4(nil, ips...)
	if len(ips) == 0 {
		return nil, errors.New("no IPv4 addresses")
	}
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	// 只交付被接收的连接，其余拨号结果在取消后关闭，避免成功连接遗留在缓冲区。
	results := make(chan result)
	var wg sync.WaitGroup
	for _, ip := range ips {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			address := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			dialer := tls.Dialer{
				NetDialer: &net.Dialer{Timeout: 5 * time.Second},
				Config: &tls.Config{
					ServerName: serverName,
					MinVersion: tls.VersionTLS12,
				},
			}
			conn, err := dialer.DialContext(dialCtx, "tcp4", address)
			select {
			case results <- result{conn: conn, err: err}:
			case <-dialCtx.Done():
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	var errs []string
	for item := range results {
		if item.err == nil && item.conn != nil {
			cancel()
			return item.conn, nil
		}
		if item.conn != nil {
			_ = item.conn.Close()
		}
		if item.err != nil && !errors.Is(item.err, context.Canceled) {
			errs = append(errs, item.err.Error())
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(errs) == 0 {
		return nil, errors.New("all IMAP IPv4 dials failed")
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func appendUniqueIPv4(base []net.IP, extra ...net.IP) []net.IP {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]net.IP, 0, len(base)+len(extra))
	for _, ip := range base {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	for _, ip := range extra {
		if ip == nil || ip.To4() == nil {
			continue
		}
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ip)
	}
	return out
}

func newICloudIMAPResolver(servers []string) *net.Resolver {
	cleaned := make([]string, 0, len(servers))
	for _, server := range servers {
		server = strings.TrimSpace(server)
		if server != "" {
			cleaned = append(cleaned, server)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{"1.1.1.1:53"}
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var lastErr error
			for _, dnsNetwork := range dnsFallbackNetworks(network) {
				for _, server := range cleaned {
					dialer := net.Dialer{Timeout: 2 * time.Second}
					conn, err := dialer.DialContext(ctx, dnsNetwork, server)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, lastErr
		},
	}
}

func lookupICloudIMAPIPv4(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("empty host")
	}
	var messages []string
	seen := make(map[string]struct{})
	var out []net.IP
	for _, server := range iCloudIMAPDNSServers {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		ips, err := lookupDNSAOverTCP(ctx, server, host)
		if err != nil {
			messages = append(messages, server+": "+err.Error())
			continue
		}
		for _, ip := range ips {
			if ip == nil || ip.To4() == nil {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ip)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	if len(messages) == 0 {
		return nil, errors.New("no DNS servers configured")
	}
	return nil, errors.New(strings.Join(messages, "; "))
}

func lookupDNSAOverTCP(ctx context.Context, server, host string) ([]net.IP, error) {
	queryID := uint16(time.Now().UnixNano())
	query, err := dnsBuildAQuery(host, queryID)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp4", server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	prefix := make([]byte, 2)
	binary.BigEndian.PutUint16(prefix, uint16(len(query)))
	if _, err := conn.Write(prefix); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, prefix); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(prefix))
	if size <= 0 || size > 65535 {
		return nil, fmt.Errorf("invalid DNS response size %d", size)
	}
	response := make([]byte, size)
	if _, err := io.ReadFull(conn, response); err != nil {
		return nil, err
	}
	return dnsParseAResponse(response, queryID)
}

func dnsBuildAQuery(host string, id uint16) ([]byte, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, errors.New("empty DNS host")
	}
	buf := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], 0x0100)
	binary.BigEndian.PutUint16(buf[4:6], 1)
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return nil, fmt.Errorf("invalid empty DNS label in %q", host)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("DNS label too long in %q", host)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	buf = binary.BigEndian.AppendUint16(buf, 1)
	return buf, nil
}

func dnsParseAResponse(message []byte, id uint16) ([]net.IP, error) {
	if len(message) < 12 {
		return nil, errors.New("short DNS response")
	}
	if binary.BigEndian.Uint16(message[0:2]) != id {
		return nil, errors.New("DNS response id mismatch")
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&0x000f != 0 {
		return nil, fmt.Errorf("DNS response rcode %d", flags&0x000f)
	}
	qdCount := int(binary.BigEndian.Uint16(message[4:6]))
	anCount := int(binary.BigEndian.Uint16(message[6:8]))
	offset := 12
	var err error
	for i := 0; i < qdCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+4 > len(message) {
			return nil, errors.New("short DNS question")
		}
		offset += 4
	}
	var out []net.IP
	for i := 0; i < anCount; i++ {
		offset, err = dnsSkipName(message, offset)
		if err != nil {
			return nil, err
		}
		if offset+10 > len(message) {
			return nil, errors.New("short DNS answer")
		}
		recordType := binary.BigEndian.Uint16(message[offset : offset+2])
		recordClass := binary.BigEndian.Uint16(message[offset+2 : offset+4])
		rdLength := int(binary.BigEndian.Uint16(message[offset+8 : offset+10]))
		offset += 10
		if offset+rdLength > len(message) {
			return nil, errors.New("short DNS rdata")
		}
		if recordType == 1 && recordClass == 1 && rdLength == 4 {
			out = append(out, net.IPv4(message[offset], message[offset+1], message[offset+2], message[offset+3]))
		}
		offset += rdLength
	}
	if len(out) == 0 {
		return nil, errors.New("DNS response has no A records")
	}
	return out, nil
}

func dnsSkipName(message []byte, offset int) (int, error) {
	for jumps := 0; ; jumps++ {
		if jumps > 128 {
			return 0, errors.New("DNS name compression loop")
		}
		if offset >= len(message) {
			return 0, errors.New("short DNS name")
		}
		length := int(message[offset])
		if length&0xc0 == 0xc0 {
			if offset+1 >= len(message) {
				return 0, errors.New("short DNS compression pointer")
			}
			return offset + 2, nil
		}
		if length&0xc0 != 0 {
			return 0, errors.New("invalid DNS label")
		}
		offset++
		if length == 0 {
			return offset, nil
		}
		if offset+length > len(message) {
			return 0, errors.New("short DNS label")
		}
		offset += length
	}
}

func dnsFallbackNetworks(network string) []string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "udp", "udp4", "udp6":
		return []string{"tcp", "udp"}
	case "tcp", "tcp4", "tcp6":
		return []string{"tcp", "udp"}
	case "":
		return []string{"tcp", "udp"}
	default:
		return []string{network, "udp", "tcp"}
	}
}

func CheckICloudIMAPLogin(ctx context.Context, email, appPassword string) error {
	email = strings.TrimSpace(email)
	appPassword = strings.TrimSpace(appPassword)
	if email == "" {
		return errCode("imap_email_missing", "请输入 iCloud 邮箱账号", false)
	}
	if appPassword == "" {
		return errCode("imap_app_password_missing", "请输入 App 专用密码", false)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, defaultICloudIMAPHost, defaultICloudIMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(25 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}

	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(email)+" "+imapQuote(appPassword))
	if err != nil {
		return errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}

	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	return nil
}

func WatchICloudIMAPExists(ctx context.Context, state LoginState, onExists func()) error {
	return WatchICloudIMAPExistsWithReady(ctx, state, nil, onExists)
}

func WatchICloudIMAPExistsWithReady(ctx context.Context, state LoginState, onReady, onExists func()) error {
	state, err := normalizeICloudIMAPState(state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, err := dialICloudIMAPTLS(ctx, state.IMAPHost, state.IMAPPort)
	if err != nil {
		return errCode("imap_connect_failed", "连接 iCloud IMAP 失败："+err.Error(), true)
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 iCloud IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "iCloud IMAP 未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(state.IMAPUsername)+" "+imapQuote(state.IMAPAppPassword))
	if err != nil {
		return errCode("imap_login_failed", "iCloud IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return errCode("imap_login_failed", "iCloud IMAP 登录失败，请确认 iCloud 邮箱账号和 App 专用密码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开 iCloud 收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_ = conn.SetDeadline(time.Time{})
	if onReady != nil {
		onReady()
	}

	tagIndex := 3
	for ctx.Err() == nil {
		tag := fmt.Sprintf("A%03d", tagIndex)
		tagIndex++
		hasNewMessage, err := imapWaitForExists(ctx, conn, reader, tag)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if hasNewMessage && onExists != nil {
			onExists()
		}
	}
	return ctx.Err()
}

func imapWaitForExists(ctx context.Context, conn net.Conn, reader *bufio.Reader, tag string) (bool, error) {
	return imapWaitForExistsWithTimeout(ctx, conn, reader, tag, imapIdleReadTimeout)
}

func imapWaitForExistsWithTimeout(ctx context.Context, conn net.Conn, reader *bufio.Reader, tag string, idleTimeout time.Duration) (bool, error) {
	if idleTimeout <= 0 {
		idleTimeout = imapIdleReadTimeout
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s IDLE\r\n", tag); err != nil {
		return false, err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "+") {
		return false, fmt.Errorf("IMAP IDLE 未进入等待状态：%s", strings.TrimSpace(line))
	}
	_ = conn.SetDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
	for ctx.Err() == nil {
		line, err = reader.ReadString('\n')
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() && ctx.Err() == nil {
				if finishErr := finishIMAPIdle(conn, reader, tag); finishErr != nil {
					return false, finishErr
				}
				return false, nil
			}
			return false, err
		}
		line = strings.TrimRight(line, "\r\n")
		if imapLineHasExistsEvent(line) {
			if err := finishIMAPIdle(conn, reader, tag); err != nil {
				return false, err
			}
			return true, nil
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			_ = conn.SetDeadline(time.Time{})
			if !imapTaggedOK([]string{line}, tag) {
				return false, fmt.Errorf("IMAP IDLE 被服务器结束：%s", strings.TrimSpace(line))
			}
			return false, nil
		}
	}
	return false, ctx.Err()
}

func finishIMAPIdle(conn net.Conn, reader *bufio.Reader, tag string) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprint(conn, "DONE\r\n"); err != nil {
		return err
	}
	lines, err := imapReadUntilTag(reader, tag)
	if err != nil {
		return err
	}
	if !imapTaggedOK(lines, tag) {
		return fmt.Errorf("IMAP IDLE 结束失败：%s", imapResponseSummary(lines))
	}
	return nil
}

func imapReadUntilTag(reader *bufio.Reader, tag string) ([]string, error) {
	lines := make([]string, 0, 4)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, nil
		}
	}
	return lines, errors.New("IMAP IDLE 响应过长")
}

func imapCommand(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, error) {
	lines, _, err := imapCommandWithLiterals(conn, reader, tag, command)
	return lines, err
}

func imapCommandWithLiterals(conn net.Conn, reader *bufio.Reader, tag, command string) ([]string, [][]byte, error) {
	if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
		return nil, nil, err
	}
	lines := make([]string, 0, 4)
	literals := make([][]byte, 0)
	for i := 0; i < 200; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			return lines, literals, err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, strings.TrimSpace(line))
		if n, ok := imapLiteralSize(line); ok {
			body := make([]byte, n)
			if _, err := io.ReadFull(reader, body); err != nil {
				return lines, literals, err
			}
			literals = append(literals, body)
		}
		if strings.HasPrefix(strings.TrimSpace(line), tag+" ") {
			return lines, literals, nil
		}
	}
	return lines, literals, errors.New("IMAP 响应过长")
}

func imapTaggedOK(lines []string, tag string) bool {
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(upper, strings.ToUpper(tag)+" ") {
			return strings.Contains(upper, " OK")
		}
	}
	return false
}

func imapQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return `"` + value + `"`
}

func imapResponseSummary(lines []string) string {
	joined := strings.Join(lines, "；")
	return trimForError([]byte(joined))
}
