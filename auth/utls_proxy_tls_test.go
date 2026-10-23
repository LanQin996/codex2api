package auth

import (
	"bytes"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestAuthHTTPProxyDialerUsesTLSForHTTPSProxy(t *testing.T) {
	tests := []struct {
		name       string
		scheme     string
		wantPrefix []byte
	}{
		{name: "http proxy sends CONNECT in plaintext", scheme: "http", wantPrefix: []byte("CONNECT ")},
		{name: "https proxy starts with TLS ClientHello", scheme: "https", wantPrefix: []byte{0x16, 0x03}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			captured := make(chan []byte, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 512)
				n, _ := conn.Read(buf)
				captured <- append([]byte(nil), buf[:n]...)
			}()

			u, err := url.Parse(tc.scheme + "://user:pass@" + ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			dialer, err := buildAuthHTTPProxyDialer(u)
			if err != nil {
				t.Fatal(err)
			}
			go func() { _, _ = dialer.Dial("tcp", "example.com:443") }()

			select {
			case data := <-captured:
				if !bytes.HasPrefix(data, tc.wantPrefix) {
					t.Fatalf("first bytes = %q, want prefix %q", data, tc.wantPrefix)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("proxy dialer sent no bytes")
			}
		})
	}
}
