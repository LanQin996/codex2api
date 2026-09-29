package credentialops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRunnerKeepsSecretsOutOfArgsAndChildConfiguration(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	script := `import fs from 'node:fs';
if (process.env.CREDENTIAL_OPS_KEY || process.env.ADMIN_SECRET || process.env.DATABASE_URL) process.exit(4);
if (process.argv.some(x => x.includes('synthetic-private'))) process.exit(5);
if (process.env.CHATGPT_LOGIN_PASSWORD !== 'synthetic-private $value----with-spaces ') process.exit(6);
if (process.env.CHATGPT_TOTP_SECRET !== 'JBSWY3DPEHPK3PXP') process.exit(7);
if (process.env.TOSUB2_PYTHON !== 'synthetic-python-path') process.exit(8);
console.error(process.env.CHATGPT_LOGIN_PASSWORD);
fs.writeFileSync(process.argv[process.argv.indexOf('--sub2api-out')+1], JSON.stringify({accounts:[{credentials:{access_token:'synthetic-at',refresh_token:'synthetic-rt'}}]}));`
	if err := os.WriteFile(filepath.Join(root, "src", "protocol-login.mjs"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIAL_OPS_KEY", "must-not-be-inherited")
	t.Setenv("ADMIN_SECRET", "must-not-be-inherited")
	t.Setenv("DATABASE_URL", "must-not-be-inherited")
	t.Setenv("TOSUB2_PYTHON", "synthetic-python-path")
	runner := Runner{Root: root}
	result, err := runner.Login(context.Background(), Login{Email: "test@example.com", Password: "synthetic-private $value----with-spaces ", TOTP: "JBSWY3DPEHPK3PXP"})
	if err != nil {
		t.Fatal(err)
	}
	if result["access_token"] != "synthetic-at" {
		t.Fatal("invalid result")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.Login(ctx, Login{Email: "test@example.com", Password: "synthetic"}); err == nil {
		t.Fatal("cancelled process ran")
	}
}
