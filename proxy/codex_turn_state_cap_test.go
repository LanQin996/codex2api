package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestCodexTurnStateOriginsCapped(t *testing.T) {
	const prefix = "cap-test-"
	remove := func() {
		codexTurnStateOrigins.Range(func(key, _ any) bool {
			if name, ok := key.(string); ok && strings.HasPrefix(name, prefix) {
				codexTurnStateOrigins.Delete(key)
			}
			return true
		})
	}
	remove()
	t.Cleanup(remove)

	account := &auth.Account{DBID: 1}
	for i := 0; i < codexTurnStateMaxOrigins+5000; i++ {
		noteCodexTurnStateProvenance(fmt.Sprintf("%s%d", prefix, i), account)
	}

	live := 0
	codexTurnStateOrigins.Range(func(key, _ any) bool {
		if name, ok := key.(string); ok && strings.HasPrefix(name, prefix) {
			live++
		}
		return true
	})
	// 最后一次清扫后最多再写入 255 条。
	if live > codexTurnStateMaxOrigins+255 {
		t.Fatalf("turn-state origins = %d, want <= %d", live, codexTurnStateMaxOrigins+255)
	}
}
