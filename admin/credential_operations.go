package admin

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/credentialops"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type credentialOperationsService struct {
	cipher  cipher.AEAD
	runner  credentialops.Runner
	login   func(context.Context, credentialops.Login) (map[string]any, error)
	once    sync.Once
	running atomic.Bool
}

func (h *Handler) credentialOperations() *credentialOperationsService {
	h.credentialOpsOnce.Do(func() {
		a, _ := credentialops.NewCipher(os.Getenv("CREDENTIAL_OPS_KEY"))
		r := credentialops.Runner{Root: strings.TrimSpace(os.Getenv("TOSUB2_ROOT")), Node: strings.TrimSpace(os.Getenv("CREDENTIAL_OPS_NODE"))}
		h.credentialOps = &credentialOperationsService{cipher: a, runner: r, login: r.Login}
	})
	return h.credentialOps
}
func (h *Handler) ListCredentialOperations(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	service := h.credentialOperations()
	rows, err := h.db.ListCredentialOperations(c.Request.Context())
	if err != nil {
		writeError(c, 500, "凭证运营状态读取失败")
		return
	}
	c.JSON(200, gin.H{"encryption_ready": service.cipher != nil, "worker_ready": service.running.Load(), "items": rows})
}
func (h *Handler) ImportCredentialOperations(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	service := h.credentialOperations()
	if service.cipher == nil || !service.running.Load() {
		writeError(c, 503, "请先配置 CREDENTIAL_OPS_KEY 和本地 TOSUB2_ROOT Worker")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 300*1024)
	var req struct {
		Content         string `json:"content"`
		ProxyURL        string `json:"proxy_url"`
		ReplaceExisting bool   `json:"replace_existing"`
	}
	if c.ShouldBindJSON(&req) != nil {
		writeError(c, 400, "导入格式错误或超过大小限制")
		return
	}
	entries, err := credentialops.Parse(req.Content)
	if err != nil {
		writeError(c, 400, err.Error())
		return
	}
	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			writeError(c, 400, "代理地址格式错误")
			return
		}
	}
	items := []database.CredentialOperation{}
	for _, entry := range entries {
		entry.ProxyURL = proxyURL
		id := uuid.NewString()
		var replace bool
		if req.ReplaceExisting {
			existing, err := h.db.FindCredentialOperationByEmail(c.Request.Context(), entry.Email)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				writeError(c, 500, "读取已有任务失败，请刷新列表后重试")
				return
			}
			if err == nil {
				id = existing.ID
				replace = true
			}
		}
		encrypted, err := credentialops.Seal(service.cipher, id, "login", entry)
		if err != nil {
			writeError(c, 500, "登录资料加密失败")
			return
		}
		var record *database.CredentialOperation
		if replace {
			err = h.db.ReplaceCredentialOperationLogin(c.Request.Context(), id, encrypted)
			if err != nil {
				writeError(c, 409, "任务执行中，未覆盖登录资料；请等待完成后重试")
				return
			}
			record, err = h.db.GetCredentialOperation(c.Request.Context(), id)
		} else {
			record, err = h.db.InsertCredentialOperation(c.Request.Context(), database.CredentialOperation{ID: id, Email: entry.Email, LoginCipher: encrypted})
		}
		if err != nil {
			writeError(c, 500, "部分任务可能已登记，请刷新列表后重试；不会重复建号")
			return
		}
		items = append(items, *record)
	}
	// Accepted is not successful import; clients must inspect each persisted state.
	c.JSON(http.StatusAccepted, gin.H{"items": items, "message": "已登记任务，登录并完成账号登记后才算导入成功"})
}
func (h *Handler) ControlCredentialOperations(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	action := c.Param("action")
	if action != "pause" && action != "resume" && action != "retry" {
		writeError(c, 400, "无效操作")
		return
	}
	if err := h.db.ControlCredentialOperation(c.Request.Context(), c.Param("operation_id"), action); err != nil {
		writeError(c, 409, "状态已变化、任务执行中或账号已暂停，请刷新后重试")
		return
	}
	c.JSON(200, gin.H{"message": "已更新任务状态"})
}

// Local worker is opt-in and supervised by the existing application lifecycle.
// No unauthenticated worker API exposes login secrets. Multiple API instances
// can run workers: durable leases and CAS fence duplicate/stale completions.
func (h *Handler) StartCredentialOperations(ctx context.Context) {
	s := h.credentialOperations()
	if s.cipher == nil || !s.runner.Ready() {
		return
	}
	s.once.Do(func() {
		go func() {
			s.running.Store(true)
			log.Print("[credential-ops] local 2FA worker started")
			defer s.running.Store(false)
			credentialops.RunPool(ctx, 5*time.Second, func(ctx context.Context) (int, error) {
				settings, err := h.db.GetCredentialOperationsSettings(ctx)
				return settings.Concurrency, err
			}, func(ctx context.Context) (func(context.Context), error) {
				job, err := h.db.ClaimCredentialOperation(ctx, uuid.NewString())
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context) { h.runCredentialOperation(ctx, s, job) }, nil
			})
		}()
	})
}

func (h *Handler) runCredentialOperation(ctx context.Context, s *credentialOperationsService, job *database.CredentialOperation) {
	ctx, cancel := context.WithTimeout(ctx, 27*time.Minute)
	defer cancel()
	fail := func(message string) {
		job.Message = message
		job.NextRun = time.Now().Add(30 * time.Minute).Unix()
		if job.ResultCipher != "" {
			job.State = "enrollment_pending"
		} else {
			job.State = "login_failed"
		}
		_ = h.db.SaveCredentialOperation(ctx, job)
	}
	var login credentialops.Login
	if credentialops.Open(s.cipher, job.ID, "login", job.LoginCipher, &login) != nil {
		fail("登录资料无法解密，请恢复原加密密钥")
		return
	}
	var existing *database.AccountRow
	if job.AccountID > 0 {
		var err error
		existing, err = h.db.GetAccountByID(ctx, job.AccountID)
		if err != nil || existing.Status == "deleted" || existing.ErrorMessage == "deleted" || !existing.Enabled || existing.Platform != "openai" || existing.Type != "oauth" {
			fail("账号不可用，停止自动操作")
			return
		}
		if !strings.EqualFold(existing.GetCredential("email"), login.Email) {
			fail("账号身份已变化，停止自动操作")
			return
		}
	}
	if job.State == "enrolled" && existing != nil {
		account, err := h.store.BuildTransientAccountByID(ctx, job.AccountID)
		if err != nil {
			fail("账号巡检准备失败")
			return
		}
		_, probeErr := proxy.FetchCodexModelsManifest(ctx, account, h.store.ResolveProxyForAccount(account), "", "", nil)
		if probeErr == nil {
			job.Failures = 0
			job.Message = "巡检正常"
			job.NextRun = time.Now().Add(30 * time.Minute).Unix()
			_ = h.db.SaveCredentialOperation(ctx, job)
			return
		}
		// The gateway helper's fixed prefix carries the actual upstream status.
		if credentialops.IsAccountBlockedResponse(probeErr.Error()) {
			_ = h.db.BlockCredentialOperation(ctx, job)
			return
		}
		authFailure := strings.HasPrefix(probeErr.Error(), "codex models upstream status 401:") || strings.HasPrefix(probeErr.Error(), "codex models upstream status 403:")
		job.NextRun = time.Now().Add(5 * time.Minute).Unix()
		job.Message = "巡检临时异常，不触发重登"
		if authFailure {
			job.Failures++
			job.Message = "巡检鉴权失败"
		}
		if !authFailure || job.Failures < 2 || !job.AutoRelogin {
			_ = h.db.SaveCredentialOperation(ctx, job)
			return
		}
		job.Snapshot = database.CredentialOperationSnapshot(existing)
	}
	var credentials map[string]any
	if job.ResultCipher != "" {
		if credentialops.Open(s.cipher, job.ID, "result", job.ResultCipher, &credentials) != nil {
			fail("登录结果无法解密，请恢复原加密密钥")
			return
		}
	} else {
		if existing != nil {
			job.Snapshot = database.CredentialOperationSnapshot(existing)
			account, err := h.store.BuildTransientAccountByID(ctx, job.AccountID)
			if err != nil {
				fail("账号代理读取失败")
				return
			}
			login.ProxyURL = h.store.ResolveProxyForAccount(account)
		} else if login.ProxyURL == "" {
			login.ProxyURL = h.store.GetProxyURL()
		}
		var err error
		credentials, err = s.login(ctx, login)
		if err != nil {
			if errors.Is(err, credentialops.ErrAccountBlocked) {
				_ = h.db.BlockCredentialOperation(ctx, job)
				return
			}
			fail("本地登录失败，请检查登录资料、代理及 Worker 配置后重试")
			return
		}
		job.ResultCipher, err = credentialops.Seal(s.cipher, job.ID, "result", credentials)
		if err != nil {
			fail("登录结果加密失败")
			return
		}
		if err = h.db.CheckpointCredentialOperation(ctx, job); err != nil {
			return
		}
		job.State = "enrollment_pending"
	}
	seed, err := credentialOperationSeed(credentials, login.Email)
	if err != nil {
		job.ResultCipher = ""
		fail("登录返回的身份或凭据不匹配，未写入账号")
		return
	}
	var updates map[string]any
	if existing != nil {
		previous := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: existing.GetCredential("access_token"), idToken: existing.GetCredential("id_token"), email: existing.GetCredential("email"), workspaceID: existing.GetCredential("workspace_id"), accountID: existing.GetCredential("account_id")})
		if effectiveWorkspaceIDFromSeed(previous) != effectiveWorkspaceIDFromSeed(seed) {
			job.ResultCipher = ""
			fail("重登工作区不匹配，未写入账号")
			return
		}
		// Empty snapshot identifies initial enrollment, including a retry after
		// the account was created. Do not overwrite that account's tokens.
		if job.Snapshot != "" {
			updates = tokenCredentialMap(seed)
		}
	} else {
		// Unlike generic reimport, a duplicate is only enrolled, not overwritten.
		h.mergeDuplicateMu.Lock()
		id, findErr := h.findOAuthIdentityDuplicate(ctx, seed, 0)
		if findErr == nil && id == 0 {
			id, findErr = h.db.InsertAccountWithCredentials(ctx, login.Email, h.newCodexAccountCredentials(seed), login.ProxyURL)
		}
		h.mergeDuplicateMu.Unlock()
		if findErr != nil {
			fail("登录成功，账号导入待重试")
			return
		}
		job.AccountID = id
	}
	job.ExpectedWorkspace = effectiveWorkspaceIDFromSeed(seed)
	if err = h.db.CompleteCredentialOperation(ctx, job, updates); err != nil {
		if errors.Is(err, database.ErrCredentialOperationStale) {
			job.ResultCipher = ""
			job.Snapshot = ""
			job.State = "enrolled"
			job.Message = "账号或任务已变化，已丢弃旧重登结果"
			job.NextRun = time.Now().Add(5 * time.Minute).Unix()
			_ = h.db.SaveCredentialOperation(ctx, job)
			return
		}
		fail("账号导入后登记尚未完成，将复用登录结果补登记")
		return
	}
	_ = h.reloadTokenAccount(ctx, job.AccountID, "credential-operations")
	h.db.InsertAccountEventAsync(job.AccountID, "credential_operations", "登录及凭证运营登记完成")
}
func credentialOperationSeed(values map[string]any, email string) (tokenCredentialSeed, error) {
	get := func(key string) string { v, _ := values[key].(string); return v }
	// Derive identity from returned tokens, not from caller-controlled email.
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: get("access_token"), refreshToken: get("refresh_token"), idToken: get("id_token"), expiresAtRaw: get("expires_at")})
	if seed.accessToken == "" || seed.refreshToken == "" || !strings.EqualFold(seed.email, email) || effectiveWorkspaceIDFromSeed(seed) == "" {
		return tokenCredentialSeed{}, errors.New("login identity mismatch")
	}
	return seed, nil
}
