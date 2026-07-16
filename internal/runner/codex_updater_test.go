package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func updaterFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	updater := filepath.Join(home, "updater")
	if err := os.WriteFile(updater, []byte(GenerateCodexUpdater()), 0755); err != nil {
		t.Fatal(err)
	}
	npm := `#!/bin/bash
set -eu
if [ "$1" = view ]; then
  [ "${NPM_VIEW_EXIT:-0}" = 0 ] || exit "$NPM_VIEW_EXIT"
  if [ "${3:-}" = time ]; then
    if [ -n "${NPM_TIMES_JSON:-}" ]; then
      echo "$NPM_TIMES_JSON"
    else
      printf '{"%s":"2020-01-01T00:00:00.000Z"}\n' "$NPM_LATEST"
    fi
  else
    echo "${NPM_LATEST}"
  fi
  exit 0
fi
prefix=""; cache=""; version="${NPM_LATEST}"
while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) prefix="$2"; shift 2;;
    --cache) cache="$2"; shift 2;;
    @openai/codex@*) version="${1##*@}"; shift;;
    *) shift;;
  esac
done
echo "$prefix|$cache" >> "$HOME/npm-calls"
[ "${NPM_INSTALL_EXIT:-0}" = 0 ] || exit "$NPM_INSTALL_EXIT"
mkdir -p "$prefix/node_modules/.bin"
[ "${NPM_MISSING_BIN:-0}" = 1 ] && exit 0
cat > "$prefix/node_modules/.bin/codex" <<EOF
#!/bin/bash
case "\${1:-}" in
  --version) echo "codex-cli ${version}";;
  --help) echo "--dangerously-bypass-approvals-and-sandbox --model --cd";;
  *) exit "${CODEX_RUN_EXIT:-0}";;
esac
EOF
chmod +x "$prefix/node_modules/.bin/codex"
`
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(npm), 0755); err != nil {
		t.Fatal(err)
	}
	return home, updater
}

func runUpdater(t *testing.T, home, updater string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(updater, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+filepath.Join(home, "fake-bin")+":"+os.Getenv("PATH"))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCodexUpdaterPromotesValidatedCandidateWithFreshCache(t *testing.T) {
	home, updater := updaterFixture(t)
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "1.2.3"}, "require"); err != nil {
		t.Fatalf("require failed: %v\n%s", err, out)
	}
	live := filepath.Join(home, ".npm-global/bin/codex")
	if out, err := exec.Command(live, "--version").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "codex-cli 1.2.3" {
		t.Fatalf("live version = %q, %v", out, err)
	}
	calls, _ := os.ReadFile(filepath.Join(home, "npm-calls"))
	if strings.Contains(string(calls), "|"+filepath.Join(home, ".npm")+"/") || !strings.Contains(string(calls), "/cache-1.2.3.") {
		t.Fatalf("candidate did not use an isolated cache: %s", calls)
	}
}

func TestCodexUpdaterRejectsSuccessfulNpmWithoutPlatformBinary(t *testing.T) {
	home, updater := updaterFixture(t)
	out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "2.0.0", "NPM_MISSING_BIN": "1"}, "require")
	if err == nil || !strings.Contains(out, "has no executable") {
		t.Fatalf("broken npm success was accepted: err=%v output=%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(home, ".npm-global/bin/codex")); !os.IsNotExist(err) {
		t.Fatalf("live executable created after failed validation: %v", err)
	}
}

func TestCodexUpdaterFailedCandidateRetainsCurrentRelease(t *testing.T) {
	home, updater := updaterFixture(t)
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "1.0.0"}, "require"); err != nil {
		t.Fatal(out, err)
	}
	_ = os.Remove(filepath.Join(home, ".npm-global/codex/state/last-check"))
	if _, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "2.0.0", "NPM_INSTALL_EXIT": "7"}, "update"); err == nil {
		t.Fatal("expected failed download")
	}
	out, _ := exec.Command(filepath.Join(home, ".npm-global/bin/codex"), "--version").Output()
	if strings.TrimSpace(string(out)) != "codex-cli 1.0.0" {
		t.Fatalf("current release changed: %s", out)
	}
}

func TestCodexUpdaterRollsBackQuarantinesAndAllowsLaterVersion(t *testing.T) {
	home, updater := updaterFixture(t)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		_ = os.Remove(filepath.Join(home, ".npm-global/codex/state/last-check"))
		if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": version}, "require"); err != nil {
			t.Fatal(out, err)
		}
	}
	out, err := runUpdater(t, home, updater, nil, "rollback-early", "2.0.0")
	if err != nil || !strings.Contains(out, "quarantined") {
		t.Fatalf("rollback failed: %v %s", err, out)
	}
	_ = os.Remove(filepath.Join(home, ".npm-global/codex/state/last-check"))
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "2.0.0"}, "update"); err != nil || !strings.Contains(out, "quarantined") {
		t.Fatalf("quarantined version not skipped: %v %s", err, out)
	}
	_ = os.Remove(filepath.Join(home, ".npm-global/codex/state/last-check"))
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "3.0.0"}, "require"); err != nil {
		t.Fatalf("later version ineligible: %v %s", err, out)
	}
}

func TestCodexUpdaterSelectsNewestReleaseAfterTwentyFourHourSoak(t *testing.T) {
	home, updater := updaterFixture(t)
	times := `{"1.0.0":"2020-01-01T00:00:00.000Z","2.0.0":"2999-01-01T00:00:00.000Z"}`
	out, err := runUpdater(t, home, updater, map[string]string{
		"NPM_LATEST":     "2.0.0",
		"NPM_TIMES_JSON": times,
	}, "require")
	if err != nil {
		t.Fatalf("require failed: %v\n%s", err, out)
	}
	got, err := exec.Command(filepath.Join(home, ".npm-global/bin/codex"), "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(got)) != "codex-cli 1.0.0" {
		t.Fatalf("soaked version = %q, %v", got, err)
	}
}

func TestCodexUpdaterChecksHealthyInstallOnlyDaily(t *testing.T) {
	home, updater := updaterFixture(t)
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "1.0.0"}, "require"); err != nil {
		t.Fatal(out, err)
	}
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "2.0.0", "NPM_VIEW_EXIT": "9"}, "update"); err != nil {
		t.Fatalf("daily gate should skip registry lookup: %v %s", err, out)
	}
}

func TestCodexUpdaterMigratesLegacyInPlaceInstallDespiteDailyGate(t *testing.T) {
	home, updater := updaterFixture(t)
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "1.0.0"}, "require"); err != nil {
		t.Fatal(out, err)
	}
	live := filepath.Join(home, ".npm-global/bin/codex")
	managed, err := filepath.EvalSymlinks(live)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, ".npm-global/lib/node_modules/@openai/codex/bin/codex.js")
	if err := os.MkdirAll(filepath.Dir(legacy), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(legacy, live); err != nil {
		t.Fatal(err)
	}
	if out, err := runUpdater(t, home, updater, map[string]string{"NPM_LATEST": "1.0.0"}, "update"); err != nil {
		t.Fatalf("legacy migration failed: %v %s", err, out)
	}
	target, err := filepath.EvalSymlinks(live)
	if err != nil || !strings.Contains(target, "/codex/releases/1.0.0/") {
		t.Fatalf("live target was not migrated: %q, %v", target, err)
	}
}
