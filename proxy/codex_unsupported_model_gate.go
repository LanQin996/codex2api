package proxy

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

// Upstream answers "The 'X' model is not supported when using Codex with a
// ChatGPT account" both when the account's plan lacks the model and when the
// outbound client Version is below the model's real minimum. The manifest's
// minimal_client_version is not reliable for the second case: gpt-6.1-sol
// declares 0.153.0 but only appears in the manifest from 0.159.0 (issue #793).
//
// A plan gate is per account, so a (account, model) cooldown routes around it.
// A version gate hits every account alike; cooling them down one by one drains
// the whole pool and keeps it drained after the version is fixed.
type codexUnsupportedGate int

const (
	codexUnsupportedPlanGate codexUnsupportedGate = iota
	codexUnsupportedVersionGate
)

func (g codexUnsupportedGate) String() string {
	if g == codexUnsupportedVersionGate {
		return "version"
	}
	return "plan"
}

// codexVersionGateMemory bounds how long a version gate seen on a top-tier
// account also explains lower-tier failures for the same model and version.
const codexVersionGateMemory = 30 * time.Minute

type codexVersionGateKey struct {
	model   string
	version string
}

var codexVersionGates = struct {
	mu   sync.Mutex
	seen map[codexVersionGateKey]time.Time
}{seen: make(map[codexVersionGateKey]time.Time)}

// classifyCodexUnsupportedModel decides which gate produced the 400. Only a
// Pro-tier account is strong evidence of a version gate: it is the top
// individual plan, and Spark, whose Pro-only gating is handled separately, is
// excluded here. Lower tiers stay plan-gated unless the same model and
// version already failed on a Pro-tier account recently, because plans below
// Pro can legitimately lack a model.
func classifyCodexUnsupportedModel(planType, model, version string, now time.Time) codexUnsupportedGate {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || isProOnlyModel(model) {
		return codexUnsupportedPlanGate
	}
	key := codexVersionGateKey{model: model, version: strings.TrimSpace(version)}
	codexVersionGates.mu.Lock()
	defer codexVersionGates.mu.Unlock()
	if auth.NormalizePlanType(planType) == "pro" {
		codexVersionGates.seen[key] = now
		return codexUnsupportedVersionGate
	}
	if seenAt, ok := codexVersionGates.seen[key]; ok {
		if now.Sub(seenAt) < codexVersionGateMemory {
			return codexUnsupportedVersionGate
		}
		delete(codexVersionGates.seen, key)
	}
	return codexUnsupportedPlanGate
}

func resetCodexVersionGates() {
	codexVersionGates.mu.Lock()
	clear(codexVersionGates.seen)
	codexVersionGates.mu.Unlock()
}

// codexOutboundClientVersion reads the Version header the failed attempt sent
// upstream. HTTP responses carry their request; WebSocket responses carry the
// handshake identity of the connection that served them.
func codexOutboundClientVersion(resp *http.Response) string {
	if resp == nil || resp.Request == nil {
		return ""
	}
	if version := strings.TrimSpace(resp.Request.Header.Get("Version")); version != "" {
		return version
	}
	return codexVersionFromUserAgent(resp.Request.Header.Get("User-Agent"), "")
}

func displayCodexOutboundVersion(version string) string {
	if version == "" {
		return "unknown"
	}
	return version
}
