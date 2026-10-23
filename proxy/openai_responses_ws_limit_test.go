package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func dialCompressedWSTestServer(t *testing.T, payload string) (*websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: true,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}))
	dialer := websocket.Dialer{EnableCompression: true}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial: %v", err)
	}
	return conn, func() {
		_ = conn.Close()
		server.Close()
	}
}

func TestReadOpenAIResponsesWSRejectsDecompressedOversize(t *testing.T) {
	payload := strings.Repeat("a", openAIResponsesWSMaxMessageBytes+1024)
	conn, cleanup := dialCompressedWSTestServer(t, payload)
	defer cleanup()

	err := readOpenAIResponsesWS(conn, func([]byte) bool { return true })
	if err != errOpenAIResponsesWSMessageTooLarge {
		t.Fatalf("err = %v, want errOpenAIResponsesWSMessageTooLarge", err)
	}
}

func TestReadOpenAIResponsesWSReplaysNormalMessage(t *testing.T) {
	conn, cleanup := dialCompressedWSTestServer(t, "hello")
	defer cleanup()

	var got string
	err := readOpenAIResponsesWS(conn, func(data []byte) bool {
		got = string(data)
		return false
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello" {
		t.Fatalf("emit got %q, want hello", got)
	}
}
