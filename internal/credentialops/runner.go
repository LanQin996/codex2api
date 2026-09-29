package credentialops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Runner struct{ Root, Node string }

func (r Runner) Ready() bool {
	if r.Root == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(r.Root, "src", "protocol-login.mjs"))
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	_, err = exec.LookPath(r.node())
	return err == nil
}
func (r Runner) node() string {
	if r.Node != "" {
		return r.Node
	}
	return "node"
}

// Login never invokes a shell and never returns stdout/stderr, which may contain
// passwords, TOTP secrets or tokens from an external protocol implementation.
func (r Runner) Login(ctx context.Context, login Login) (map[string]any, error) {
	if !r.Ready() {
		return nil, errors.New("local login worker is not configured")
	}
	root, err := filepath.Abs(r.Root)
	if err != nil {
		return nil, errors.New("invalid worker directory")
	}
	dir, err := os.MkdirTemp("", "codex2api-credential-ops-")
	if err != nil {
		return nil, errors.New("worker temporary directory unavailable")
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "oauth.json")
	// Pre-create at 0600. The enclosing temp directory is private on Unix.
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("worker output unavailable")
	}
	f.Close()
	args := []string{filepath.Join(root, "src", "protocol-login.mjs"), "--email", login.Email, "--output-mode", "sub2api", "--sub2api-out", output}
	if login.ProxyURL != "" {
		args = append(args, "--proxy", login.ProxyURL)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.node(), args...)
	cmd.Dir = root
	cmd.WaitDelay = 5 * time.Second
	// Explicit allowlist: do not pass database/admin/encryption keys to the adapter.
	for _, name := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "TOSUB2_PYTHON"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CHATGPT_LOGIN_PASSWORD="+login.Password, "CHATGPT_TOTP_SECRET="+login.TOTP)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		return nil, errors.New("local login failed; check worker configuration or login details")
	}
	file, err := os.Open(output)
	if err != nil {
		return nil, errors.New("worker did not produce credentials")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("invalid worker output")
	}
	defer clear(data)
	var payload struct {
		Accounts []struct {
			Credentials map[string]any `json:"credentials"`
		} `json:"accounts"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Accounts) != 1 {
		return nil, errors.New("worker must return exactly one account")
	}
	credentials := payload.Accounts[0].Credentials
	for _, key := range []string{"access_token", "refresh_token"} {
		value, _ := credentials[key].(string)
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("worker credentials are incomplete")
		}
	}
	return credentials, nil
}
