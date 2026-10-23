package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestAccountBoundEgressAgentRegistration(t *testing.T) {
	var direct, bound, inherited atomic.Int32
	server := func(label string, count *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			fmt.Fprintf(w, `{"task_id":%q}`, label)
		}))
		t.Cleanup(s.Close)
		return s
	}
	target := server("direct", &direct)
	boundProxy := server("bound", &bound)
	envProxy := server("environment", &inherited)
	oldBase, oldTransport := agentIdentityAuthAPIBase, http.DefaultTransport
	agentIdentityAuthAPIBase = target.URL
	transport := oldTransport.(*http.Transport).Clone()
	envURL, _ := url.Parse(envProxy.URL)
	transport.Proxy = http.ProxyURL(envURL)
	http.DefaultTransport = transport
	t.Cleanup(func() {
		agentIdentityAuthAPIBase = oldBase
		http.DefaultTransport = oldTransport
		transport.CloseIdleConnections()
	})
	_, encoded := newTestAgentKey(t)
	key, _ := parseAgentIdentityPrivateKey(encoded)
	runtimeKey := agentIdentityRuntimeKey{runtimeID: "agent-local-test", privateKey: key}
	for _, test := range []struct {
		ctx  context.Context
		want string
	}{
		{context.Background(), "environment"},
		{WithAccountBoundEgress(context.Background(), ""), "direct"},
		{WithAccountBoundEgress(context.Background(), boundProxy.URL), "bound"},
	} {
		got, err := registerAgentIdentityTask(test.ctx, runtimeKey, "")
		if err != nil || got != test.want {
			t.Errorf("task route = %q, %v; want %q", got, err, test.want)
		}
	}
	if _, err := registerAgentIdentityTask(WithAccountBoundEgress(context.Background(), "socks5h://"), runtimeKey, ""); err == nil {
		t.Error("invalid bound proxy did not fail")
	}
	if direct.Load() != 1 || bound.Load() != 1 || inherited.Load() != 1 {
		t.Errorf("captures direct=%d bound=%d environment=%d", direct.Load(), bound.Load(), inherited.Load())
	}
}
