package vault

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireSopsAndAge skips the test if sops or age-keygen are not on PATH.
// The bootstrap path shells out to both, so there's no faithful way to
// unit-test Set without them installed.
func requireSopsAndAge(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH — skipping integration test", bin)
		}
	}
}

// setupVaultDir creates a temp dir with a fresh age keypair and a
// .sops.yaml pointing at it, then chdirs there. Returns a cleanup func.
func setupVaultDir(t *testing.T) func() {
	t.Helper()

	dir := t.TempDir()
	keysPath := filepath.Join(dir, "keys.txt")

	out, err := exec.Command("age-keygen", "-o", keysPath).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, out)
	}

	data, err := os.ReadFile(keysPath)
	if err != nil {
		t.Fatalf("reading keys: %v", err)
	}
	pubKey := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# public key:") {
			pubKey = strings.TrimSpace(strings.TrimPrefix(line, "# public key:"))
			break
		}
	}
	if pubKey == "" {
		t.Fatalf("no public key in %s", keysPath)
	}

	sopsCfg := "creation_rules:\n  - path_regex: secrets\\.sops\\.yaml\n    age: " + pubKey + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(sopsCfg), 0644); err != nil {
		t.Fatalf("write .sops.yaml: %v", err)
	}

	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	prevKey := os.Getenv("SOPS_AGE_KEY_FILE")
	os.Setenv("SOPS_AGE_KEY_FILE", keysPath)

	return func() {
		_ = os.Chdir(prevCwd)
		if prevKey == "" {
			os.Unsetenv("SOPS_AGE_KEY_FILE")
		} else {
			os.Setenv("SOPS_AGE_KEY_FILE", prevKey)
		}
	}
}

func TestSet_BootstrapsMissingFile(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	if _, err := os.Stat(secretsFile); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist before Set", secretsFile)
	}

	if err := Set("clementine", "DISCORD_TOKEN=abc123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	data, err := os.ReadFile(secretsFile)
	if err != nil {
		t.Fatalf("secrets file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "clementine:") {
		t.Errorf("expected 'clementine:' key in encrypted file, got:\n%s", content)
	}
	if !strings.Contains(content, "DISCORD_TOKEN:") {
		t.Errorf("expected 'DISCORD_TOKEN:' in encrypted file, got:\n%s", content)
	}
	if strings.Contains(content, "abc123") {
		t.Errorf("plaintext value leaked into encrypted file:\n%s", content)
	}
	if !strings.Contains(content, "ENC[") {
		t.Errorf("expected sops ENC[...] markers in encrypted file, got:\n%s", content)
	}
}

func TestSet_AddsKeyToExistingFile(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	if err := Set("v1", "A=1"); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := Set("v1", "B=2"); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	out, err := exec.Command("sops", "-d", secretsFile).CombinedOutput()
	if err != nil {
		t.Fatalf("sops -d: %v\n%s", err, out)
	}
	plain := string(out)
	if !strings.Contains(plain, "A: \"1\"") && !strings.Contains(plain, "A: '1'") && !strings.Contains(plain, "A: 1") {
		t.Errorf("expected A=1 after decrypt, got:\n%s", plain)
	}
	if !strings.Contains(plain, "B: \"2\"") && !strings.Contains(plain, "B: '2'") && !strings.Contains(plain, "B: 2") {
		t.Errorf("expected B=2 after decrypt, got:\n%s", plain)
	}
}

func TestRenameVault_ReEncryptsUnderNewName(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	// Legacy underscore name that ValidateVaultName now rejects: Set won't make
	// it, so seed it straight via sops (mirrors a pre-existing illegal vault).
	if err := Set("seed", "K=1"); err != nil {
		t.Fatalf("Set seed: %v", err)
	}
	if out, err := exec.Command("sops", "--set",
		`["vaults"]["dev_to"]["DEV_TO_API_KEY"] "tok123"`, secretsFile).CombinedOutput(); err != nil {
		t.Fatalf("seed dev_to: %v\n%s", err, out)
	}
	if err := RenameVault("dev_to", "dev-to"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	out, err := exec.Command("sops", "-d", secretsFile).CombinedOutput()
	if err != nil {
		t.Fatalf("sops -d: %v\n%s", err, out)
	}
	plain := string(out)
	if !strings.Contains(plain, "dev-to:") {
		t.Errorf("expected new vault dev-to: after rename, got:\n%s", plain)
	}
	if strings.Contains(plain, "dev_to:") {
		t.Errorf("old vault dev_to: still present after rename:\n%s", plain)
	}
	if !strings.Contains(plain, "tok123") {
		t.Errorf("secret value not preserved through rename:\n%s", plain)
	}
}

func TestRenameVault_RefusesOverwrite(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	if err := Set("a", "K=1"); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := Set("b", "K=2"); err != nil {
		t.Fatalf("Set b: %v", err)
	}
	if err := RenameVault("a", "b"); err == nil {
		t.Fatal("expected error renaming onto an existing vault, got nil")
	}
}

func TestRenameKey_ReEncryptsUnderNewKey(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	if err := Set("v1", "OLD_KEY=val42"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := RenameKey("v1", "OLD_KEY", "NEW_KEY"); err != nil {
		t.Fatalf("RenameKey: %v", err)
	}

	out, err := exec.Command("sops", "-d", secretsFile).CombinedOutput()
	if err != nil {
		t.Fatalf("sops -d: %v\n%s", err, out)
	}
	plain := string(out)
	if !strings.Contains(plain, "NEW_KEY:") || strings.Contains(plain, "OLD_KEY:") {
		t.Errorf("expected OLD_KEY renamed to NEW_KEY, got:\n%s", plain)
	}
	if !strings.Contains(plain, "val42") {
		t.Errorf("value not preserved through key rename:\n%s", plain)
	}
}

func TestSet_RejectsMalformedKeyval(t *testing.T) {
	cleanup := setupVaultDir(t)
	defer cleanup()

	err := Set("v1", "no-equals-sign")
	if err == nil {
		t.Fatal("expected error for missing =, got nil")
	}
	if !strings.Contains(err.Error(), "KEY=value") {
		t.Errorf("expected error to mention 'KEY=value', got: %v", err)
	}
}

func TestSet_BlocksShellInjectionKeyName(t *testing.T) {
	dir := t.TempDir()
	pwned := filepath.Join(dir, "pwned-marker")
	envLine := "export MY_KEY; touch " + pwned + "\n"
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte(envLine), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "source "+envFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("source .env: %v\n%s", err, out)
	}
	if _, err := os.Stat(pwned); err != nil {
		t.Fatal("sanity: malicious export line must execute injected command when sourced")
	}

	cleanup := setupVaultDir(t)
	defer cleanup()
	err := Set("v1", "MY_KEY; touch /tmp/ignored=secret")
	if err == nil {
		t.Fatal("Set should reject malicious key name before it reaches WriteEnvFile")
	}
	if !strings.Contains(err.Error(), "valid env var name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateSecretKey_RejectsUnsafeNames(t *testing.T) {
	cleanup := setupVaultDir(t)
	defer cleanup()

	for _, key := range []string{
		"MY_KEY; curl https://evil.example",
		"$(id)",
		"123BAD",
		"",
		"has-dash",
		"has space",
	} {
		t.Run(key, func(t *testing.T) {
			if err := Set("v1", key+"=secret"); err == nil {
				t.Fatalf("Set(%q) expected error", key)
			} else if !strings.Contains(err.Error(), "valid env var name") {
				t.Fatalf("Set(%q) error = %v, want validation message", key, err)
			}
		})
	}
}

func TestGet_RejectsInvalidKeyName(t *testing.T) {
	cleanup := setupVaultDir(t)
	defer cleanup()

	err := Get("v1", "bad;key")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "valid env var name") {
		t.Errorf("got: %v", err)
	}
}

func TestDelete_RejectsInvalidKeyName(t *testing.T) {
	cleanup := setupVaultDir(t)
	defer cleanup()

	err := Delete("v1", "$(rm)")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "valid env var name") {
		t.Errorf("got: %v", err)
	}
}

func TestJqEscape(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`simple`, `simple`},
		{`with"quote`, `with\"quote`},
		{`back\slash`, `back\\slash`},
		{`C:\Users\token`, `C:\\Users\\token`},
		{`\n literal`, `\\n literal`},
		{`both\"`, `both\\\"`},
	}
	for _, c := range cases {
		got := jqEscape(c.input)
		if got != c.want {
			t.Errorf("jqEscape(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestSet_BackslashInValue(t *testing.T) {
	requireSopsAndAge(t)
	cleanup := setupVaultDir(t)
	defer cleanup()

	if err := Set("myvault", `PATH=C:\Users\token`); err != nil {
		t.Fatalf("Set with backslash: %v", err)
	}

	secrets, err := DecryptForAgent("", []string{"myvault"})
	if err != nil {
		t.Fatalf("DecryptForAgent: %v", err)
	}
	want := `C:\Users\token`
	if got := secrets["myvault.PATH"]; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// requireAgeKeygen skips if age-keygen is not on PATH.
func requireAgeKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("age-keygen"); err != nil {
		t.Skip("age-keygen not on PATH — skipping integration test")
	}
}

func TestInit_FreshInit(t *testing.T) {
	requireAgeKeygen(t)

	tmpHome := t.TempDir()
	tmpDir := t.TempDir()

	prevHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", prevHome)

	prevCwd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prevCwd) }()

	if err := Init(); err != nil {
		t.Fatalf("Init fresh: %v", err)
	}

	keysPath := filepath.Join(tmpHome, defaultAgeKeysPath)
	if _, err := os.Stat(keysPath); err != nil {
		t.Errorf("keys.txt not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".sops.yaml")); err != nil {
		t.Errorf(".sops.yaml not created: %v", err)
	}
}

func TestInit_ReuseExistingKey(t *testing.T) {
	requireAgeKeygen(t)

	tmpHome := t.TempDir()
	tmpDir := t.TempDir()

	keysPath := filepath.Join(tmpHome, defaultAgeKeysPath)
	if err := os.MkdirAll(filepath.Dir(keysPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	out, err := exec.Command("age-keygen", "-o", keysPath).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen setup: %v\n%s", err, out)
	}

	data, _ := os.ReadFile(keysPath)
	originalContent := string(data)

	prevHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", prevHome)

	prevCwd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prevCwd) }()

	if err := Init(); err != nil {
		t.Fatalf("Init reuse: %v", err)
	}

	// Key file must be unchanged — age-keygen was not re-run.
	after, _ := os.ReadFile(keysPath)
	if string(after) != originalContent {
		t.Error("keys.txt was overwritten; Init should reuse existing key")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".sops.yaml")); err != nil {
		t.Errorf(".sops.yaml not created: %v", err)
	}
}

// TestFlatSecrets pins the vault-prefix stripping used for .env exports:
// "vaultName.keyName" becomes bare "keyName", unqualified keys pass through,
// and only the first dot delimits (values like keys with dots keep the rest).
func TestFlatSecrets(t *testing.T) {
	in := map[string]string{
		"github.GH_TOKEN":      "tok",
		"discord-lead.TOKEN.X": "weird", // only the first dot is the vault delimiter
		"BARE_KEY":             "v",
	}
	got := FlatSecrets(in)
	want := map[string]string{
		"GH_TOKEN": "tok",
		"TOKEN.X":  "weird",
		"BARE_KEY": "v",
	}
	if len(got) != len(want) {
		t.Fatalf("FlatSecrets = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("FlatSecrets[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestBug122_YqInjectionEnumeratesAllVaults(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not on PATH")
	}
	decrypted := `vaults:
  admin:
    ROOT: topsecret
  lead:
    TOKEN: ok
`
	// When embedded in DecryptForAgent's fmt.Sprintf, this yq alternative operator
	// falls back to the entire .vaults subtree when "missing" does not exist.
	malicious := `missing // .vaults`
	expr := ".vaults." + malicious + " | keys"
	out, err := runYQ(expr, decrypted)
	if err != nil {
		t.Fatalf("runYQ: %v", err)
	}
	if !strings.Contains(out, "admin") || !strings.Contains(out, "lead") {
		t.Fatalf("malicious vault name should enumerate all vaults, got:\n%s", out)
	}

	exprLead := ".vaults.lead | keys"
	outLead, err := runYQ(exprLead, decrypted)
	if err != nil {
		t.Fatalf("runYQ lead: %v", err)
	}
	if strings.Contains(outLead, "admin") {
		t.Fatalf("valid vault name should not expose admin vault, got:\n%s", outLead)
	}
}

func TestValidateVaultName_RejectsInjection(t *testing.T) {
	for _, name := range []string{
		"missing // .vaults",
		"shared // .vaults",
		"UPPER",
		"has_underscore",
		"has.dot",
		"",
	} {
		if err := ValidateVaultName(name); err == nil {
			t.Errorf("ValidateVaultName(%q) expected error", name)
		}
	}
	if err := ValidateVaultName("github"); err != nil {
		t.Errorf("ValidateVaultName(github): %v", err)
	}
}

func TestDecryptForAgent_RejectsInvalidVaultName(t *testing.T) {
	_, err := DecryptForAgent("lead", []string{"missing // .vaults"})
	if err == nil {
		t.Fatal("expected error for malicious vault name")
	}
	if !strings.Contains(err.Error(), "vault name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSet_RejectsInvalidVaultName(t *testing.T) {
	cleanup := setupVaultDir(t)
	defer cleanup()
	err := Set("bad // vault", "KEY=value")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "vault name") {
		t.Fatalf("unexpected error: %v", err)
	}
}
