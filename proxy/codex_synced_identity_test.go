package proxy

import (
	"github.com/codex2api/auth"
	"net/http"
	"strings"
	"testing"
)

func TestCodexIdentityPoolUsesSyncedVersionInPreserveMode(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.ClientCompatMode = ClientCompatModePreserve
	s.CodexSyncedCLIVersion = "0.155.1"
	s.CodexUserAgentConfig = `{"client_kind":"codex-tui","mode":"pool"}`
	ApplyRuntimeSettings(s)
	for _, id := range []int64{19, 20, 21, 35, 36} {
		ua, v, _ := resolveCodexOutboundClientHeaders(&auth.Account{DBID: id}, "", nil, http.Header{})
		if !codexVersionAtLeast(v, "0.155.1") || !strings.Contains(ua, "/"+v) {
			t.Fatalf("account=%d version=%s ua=%s", id, v, ua)
		}
	}
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.150.0")
	headers.Set("Version", "0.150.0")
	headers.Set("Originator", "codex_cli_rs")
	ua, v, _ := resolveCodexOutboundClientHeaders(nil, "", nil, headers)
	if ua != "codex_cli_rs/0.150.0" || v != "0.150.0" {
		t.Fatal("preserve mode rewrote an actual client")
	}
}
