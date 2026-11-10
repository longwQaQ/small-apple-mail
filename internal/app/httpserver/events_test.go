package httpserver

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEventStreamInitialConnectionAndReconnect(t *testing.T) {
	server, _ := testServer(t)
	cookie, _ := login(t, server)
	hub := server.Config.Handler.(*Server).hub
	for i := 0; i < 500; i++ {
		hub.Publish("job.completed", i)
	}
	for _, tt := range []struct {
		name, cursor string
		wantID       int64
	}{
		{"首次连接只接收实时事件", "", 501},
		{"重连补发游标后的事件", "498", 499},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/admin/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(cookie)
			if tt.cursor != "" {
				req.Header.Set("Last-Event-ID", tt.cursor)
			}
			res, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("事件流响应异常：status=%d", res.StatusCode)
			}
			if tt.cursor == "" {
				hub.Publish("mailbox.changed", nil)
			}
			scanner := bufio.NewScanner(res.Body)
			for scanner.Scan() {
				if id, ok := strings.CutPrefix(scanner.Text(), "id: "); ok {
					if id != strconv.FormatInt(tt.wantID, 10) {
						t.Fatalf("首条事件 ID=%s，期望 %d", id, tt.wantID)
					}
					return
				}
			}
			t.Fatalf("未收到事件：%v", scanner.Err())
		})
	}
}
