package wsrelay

import (
	"context"
	"net/http"
	"testing"
)

func TestWebsocketResponseToHTTPCarriesHandshakeIdentity(t *testing.T) {
	resp := websocketResponseToHTTP(context.Background(), &WsResponse{
		readErrChan:       make(chan error, 1),
		upstreamUserAgent: "codex-tui/0.160.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.160.0)",
		upstreamVersion:   "0.160.0",
	}, http.StatusOK, nil)
	defer resp.Body.Close()
	if resp.Request == nil {
		t.Fatal("websocket response lost the outbound handshake identity")
	}
	if got := resp.Request.Header.Get("Version"); got != "0.160.0" {
		t.Fatalf("Version = %q, want handshake value", got)
	}
	if got := resp.Request.Header.Get("User-Agent"); got == "" {
		t.Fatal("User-Agent missing from outbound identity")
	}

	anonymous := websocketResponseToHTTP(context.Background(), &WsResponse{readErrChan: make(chan error, 1)}, http.StatusOK, nil)
	defer anonymous.Body.Close()
	if anonymous.Request != nil {
		t.Fatalf("Request = %#v, want nil without a recorded handshake identity", anonymous.Request)
	}
}
