package protocol

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtocolHTTPErrorsDoNotExposeCredentials(t *testing.T) {
	for _, body := range []string{
		`{"token":"test-private-token","password":"test-private-password","cookies":["test-private-cookie"],"message":"temporary failure"}`,
		`<html>upstream error: token=test-private-token</html>`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(server.Close)
		apple := &AppleAuthClient{httpClient: server.Client()}
		_, _, appleErr := apple.do(context.Background(), &appleAuthSession{}, http.MethodGet, server.URL, nil, nil, nil, false)
		cloud := &ICloudClient{client: server.Client()}
		cloudErr := cloud.callEnvelope(context.Background(), ICloudSession{}, http.MethodGet, server.URL, nil, nil)
		for _, err := range []error{appleErr, cloudErr} {
			if err == nil || strings.Contains(err.Error(), "test-private-") || !strings.Contains(err.Error(), "502") {
				t.Fatalf("协议错误未安全保留诊断信息: %v", err)
			}
		}
	}
}
