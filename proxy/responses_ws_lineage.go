package proxy

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const responsesWSLineageMaxEntries = 256
const responsesWSLineageMaxIDBytes = 1024

type responsesWSLineageEntry struct {
	responseID string
	owner      string
	scope      string
}

// A response's stream is an ordering lane, not its conversation lineage. This
// connection-owned index permits forks to read a committed parent from another
// lane without relaxing the normal cache's API-key, group or transport scopes.
// It retains identifiers only; snapshots remain subject to their own limits.
type responsesWSLineage struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	pending map[string]chan struct{}
}

func newResponsesWSLineage() *responsesWSLineage {
	return &responsesWSLineage{entries: make(map[string]*list.Element), pending: make(map[string]chan struct{})}
}

// Reuse the authorization and database scope dimensions used by the existing
// native owner. Body-derived conversation/stream dimensions are deliberately
// absent: possession of this live connection is the additional fork boundary.
func responsesWSLineageScope(c *gin.Context, identity requestSessionIdentity, databaseScope string) string {
	keyIdentity := deterministicPromptCacheKey(strings.TrimPrefix(strings.TrimSpace(downstreamAuthorizationHeader(c.Request)), "Bearer "), nil)
	parts := []string{strconv.FormatInt(requestAPIKeyID(c), 10), keyIdentity,
		responsesWSSessionPreemptScopeHash(c, identity), databaseScope}
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

func (lineage *responsesWSLineage) lookup(ctx context.Context, responseID, scope string) (string, error) {
	if lineage == nil || responseID == "" || len(responseID) > responsesWSLineageMaxIDBytes || scope == "" {
		return "", nil
	}
	key := scope + "\x00" + responseID
	lineage.mu.Lock()
	done := lineage.pending[key]
	lineage.mu.Unlock()
	if done != nil {
		timer := time.NewTimer(nativeWSCommitWait)
		defer timer.Stop()
		select {
		case <-done:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return "", &ResponsesContinuationLostError{Reason: "context_commit_pending"}
		}
	}
	lineage.mu.Lock()
	defer lineage.mu.Unlock()
	element := lineage.entries[responseID]
	if element == nil {
		return "", nil
	}
	entry := element.Value.(responsesWSLineageEntry)
	if entry.scope != scope {
		return "", nil
	}
	lineage.order.MoveToFront(element)
	return entry.owner, nil
}

// Publish a commit barrier before the successful terminal is visible. A fork
// may arrive immediately after that write, before its parent's cache is stored.
// The barrier contains no owner and does not grant access to an uncommitted ID.
func (lineage *responsesWSLineage) beginCommit(responseID, scope string) func() {
	if lineage == nil || responseID == "" || len(responseID) > responsesWSLineageMaxIDBytes || scope == "" {
		return func() {}
	}
	key := scope + "\x00" + responseID
	lineage.mu.Lock()
	if lineage.pending[key] != nil || len(lineage.pending) >= responsesWSLineageMaxEntries {
		lineage.mu.Unlock()
		return func() {}
	}
	done := make(chan struct{})
	lineage.pending[key] = done
	lineage.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lineage.mu.Lock()
			delete(lineage.pending, key)
			close(done)
			lineage.mu.Unlock()
		})
	}
}

// Call only after a completed/incomplete response and its context have passed
// the downstream commit boundary. Merely seeing response.created is not proof.
func (lineage *responsesWSLineage) remember(responseID, owner, scope string) {
	if lineage == nil || responseID == "" || len(responseID) > responsesWSLineageMaxIDBytes || scope == "" || !strings.HasPrefix(owner, nativeWSCachePrefix) {
		return
	}
	lineage.mu.Lock()
	defer lineage.mu.Unlock()
	entry := responsesWSLineageEntry{responseID: responseID, owner: owner, scope: scope}
	if element := lineage.entries[responseID]; element != nil {
		element.Value = entry
		lineage.order.MoveToFront(element)
		return
	}
	lineage.entries[responseID] = lineage.order.PushFront(entry)
	if lineage.order.Len() > responsesWSLineageMaxEntries {
		oldest := lineage.order.Back()
		delete(lineage.entries, oldest.Value.(responsesWSLineageEntry).responseID)
		lineage.order.Remove(oldest)
	}
}
