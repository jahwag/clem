package vault

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

type avStdinCall struct {
	stdin string
	args  []string
}

func withAVRunStdin(t *testing.T, fn func(stdin string, args ...string) ([]byte, error)) *[]avStdinCall {
	t.Helper()
	var calls []avStdinCall
	orig := avRunStdin
	avRunStdin = func(stdin string, args ...string) ([]byte, error) {
		calls = append(calls, avStdinCall{stdin, args})
		return fn(stdin, args...)
	}
	t.Cleanup(func() { avRunStdin = orig })
	return &calls
}

var errStub = errors.New("stub failure")

// stubAV replaces avRun for a test, recording invocations and returning a
// canned response keyed by the first arg ("vault"/"agent").
type avCall struct {
	env  []string
	args []string
}

func withAVRun(t *testing.T, fn func(env []string, args ...string) ([]byte, error)) *[]avCall {
	t.Helper()
	var calls []avCall
	orig := avRun
	avRun = func(env []string, args ...string) ([]byte, error) {
		calls = append(calls, avCall{env, args})
		return fn(env, args...)
	}
	t.Cleanup(func() { avRun = orig })
	return &calls
}

func TestSeedVault_CreatesAndSetsCredentials(t *testing.T) {
	calls := withAVRun(t, func(env []string, args ...string) ([]byte, error) { return nil, nil })
	err := SeedVault("http://127.0.0.1:14321", "slack",
		map[string]string{"SLACK_MCP_XOXP_TOKEN": "xoxp-secret"})
	if err != nil {
		t.Fatalf("SeedVault: %v", err)
	}
	var sawCreate, sawSet bool
	for _, c := range *calls {
		joined := strings.Join(c.args, " ")
		if joined == "vault create slack" {
			sawCreate = true
		}
		if strings.HasPrefix(joined, "vault credential set SLACK_MCP_XOXP_TOKEN=xoxp-secret --vault slack") {
			sawSet = true
		}
		// Owner-session model: admin ops must NEVER set AGENT_VAULT_TOKEN, or the
		// CLI switches to agent-token auth and the operation is denied.
		for _, e := range c.env {
			if strings.HasPrefix(e, "AGENT_VAULT_TOKEN=") {
				t.Errorf("admin op must not set AGENT_VAULT_TOKEN, got %q", e)
			}
		}
	}
	if !sawCreate || !sawSet {
		t.Errorf("expected create + credential set calls, got %+v", *calls)
	}
}

func TestEnsureAgentIdentity_NoAccessProxyRoleAndToken(t *testing.T) {
	calls := withAVRun(t, func(env []string, args ...string) ([]byte, error) {
		return []byte("av_agt_minted123\n"), nil
	})
	tok, err := EnsureAgentIdentity("http://127.0.0.1:14321", "acme-lead", []string{"anthropic", "slack"})
	if err != nil {
		t.Fatalf("EnsureAgentIdentity: %v", err)
	}
	if tok != "av_agt_minted123" {
		t.Errorf("token=%q, want av_agt_minted123", tok)
	}
	joined := strings.Join((*calls)[0].args, " ")
	// Inject-only guarantee: instance role no-access + vault role proxy ONLY.
	for _, want := range []string{
		"agent create acme-lead",
		"--role no-access",
		"--vault anthropic:proxy",
		"--vault slack:proxy",
		"--token-only",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("agent create missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, ":member") || strings.Contains(joined, ":admin") {
		t.Errorf("must never grant member/admin role: %s", joined)
	}
}

func TestEnsureAgentIdentity_RotatesWhenExists(t *testing.T) {
	n := 0
	withAVRun(t, func(env []string, args ...string) ([]byte, error) {
		n++
		if args[1] == "create" {
			return []byte("error: agent already exists"), errStub
		}
		return []byte("av_agt_rotated456\n"), nil
	})
	tok, err := EnsureAgentIdentity("http://127.0.0.1:14321", "acme-lead", []string{"anthropic"})
	if err != nil {
		t.Fatalf("EnsureAgentIdentity: %v", err)
	}
	if tok != "av_agt_rotated456" {
		t.Errorf("token=%q, want rotated token", tok)
	}
}

func TestHealth_OKAnd500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := Health(srv.URL); err != nil {
		t.Errorf("Health should pass on 200: %v", err)
	}
	// Unreachable address must error.
	if err := Health("http://127.0.0.1:1"); err == nil {
		t.Error("Health should fail against an unreachable address")
	}
}

func TestFetchCA_WritesCert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/mitm/ca.pem" {
			_, _ = w.Write([]byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dest := t.TempDir() + "/ca.pem"
	if err := FetchCA(srv.URL, dest); err != nil {
		t.Fatalf("FetchCA: %v", err)
	}
	data, _ := os.ReadFile(dest)
	if !strings.Contains(string(data), "BEGIN CERTIFICATE") {
		t.Errorf("CA not written correctly: %q", data)
	}
}

func TestSeedVault_SanitizesVaultName(t *testing.T) {
	calls := withAVRun(t, func(env []string, args ...string) ([]byte, error) { return nil, nil })
	// sops vault name with an underscore — agent-vault rejects '_'.
	if err := SeedVault("http://127.0.0.1:14321", "dev_to", map[string]string{"DEV_TO_API_KEY": "k"}); err != nil {
		t.Fatalf("SeedVault: %v", err)
	}
	for _, c := range *calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, "dev_to") {
			t.Errorf("vault name must be sanitized to dev-to, got %q", joined)
		}
	}
	var sawCreate bool
	for _, c := range *calls {
		if strings.Join(c.args, " ") == "vault create dev-to" {
			sawCreate = true
		}
	}
	if !sawCreate {
		t.Errorf("expected `vault create dev-to`, got %+v", *calls)
	}
}

func TestEnsureAgentIdentity_SanitizesVaultName(t *testing.T) {
	calls := withAVRun(t, func(env []string, args ...string) ([]byte, error) {
		return []byte("av_agt_x\n"), nil
	})
	if _, err := EnsureAgentIdentity("http://127.0.0.1:14321", "ag", []string{"dev_to"}); err != nil {
		t.Fatalf("EnsureAgentIdentity: %v", err)
	}
	joined := strings.Join((*calls)[0].args, " ")
	if !strings.Contains(joined, "--vault dev-to:proxy") || strings.Contains(joined, "dev_to") {
		t.Errorf("vault should be sanitized to dev-to:proxy, got %s", joined)
	}
}

func TestEnsureOwner_LoginSucceedsNoRegister(t *testing.T) {
	calls := withAVRunStdin(t, func(stdin string, args ...string) ([]byte, error) {
		return nil, nil // login succeeds
	})
	if err := EnsureOwner("http://127.0.0.1:14321", "owner@example.com", "pw123"); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected only login (1 call), got %d: %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	joined := strings.Join(c.args, " ")
	for _, want := range []string{"auth login", "--email owner@example.com", "--password-stdin", "--address http://127.0.0.1:14321"} {
		if !strings.Contains(joined, want) {
			t.Errorf("login missing %q: %s", want, joined)
		}
	}
	if strings.TrimSpace(c.stdin) != "pw123" {
		t.Errorf("password must be fed via stdin, got %q", c.stdin)
	}
}

func TestEnsureOwner_RegistersWhenLoginFails(t *testing.T) {
	calls := withAVRunStdin(t, func(stdin string, args ...string) ([]byte, error) {
		if args[1] == "login" {
			return []byte("not logged in"), errStub
		}
		return nil, nil // register succeeds
	})
	if err := EnsureOwner("http://127.0.0.1:14321", "owner@example.com", "pw123"); err != nil {
		t.Fatalf("EnsureOwner: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected login then register (2 calls), got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[1].args[1] != "register" {
		t.Errorf("second call must be register, got %v", (*calls)[1].args)
	}
}

func TestEnsureOwner_ErrorsWhenBothFail(t *testing.T) {
	withAVRunStdin(t, func(stdin string, args ...string) ([]byte, error) {
		return []byte("boom"), errStub
	})
	if err := EnsureOwner("http://127.0.0.1:14321", "owner@example.com", "pw123"); err == nil {
		t.Error("expected error when login and register both fail")
	}
}

func TestApplyServices_BuildsCorrectFlagsPerAuthType(t *testing.T) {
	calls := withAVRun(t, func(env []string, args ...string) ([]byte, error) { return nil, nil })
	services := []config.Service{
		{Name: "llm-gateway", Host: "openrouter.ai", AuthType: "bearer", TokenKey: "OR_KEY"},
		{Name: "github", Host: "github.com", AuthType: "basic", UsernameKey: "GIT_USER", PasswordKey: "GH_TOKEN"},
		{Name: "typefully", Host: "api.typefully.com", AuthType: "api-key", APIKeyKey: "TF_KEY", APIKeyHeader: "X-API-KEY", APIKeyPrefix: "tok-"},
	}
	// Services apply to the agent's consolidated vault (sanitized name).
	if err := ApplyServices("http://127.0.0.1:14321", "team_worker", services); err != nil {
		t.Fatalf("ApplyServices: %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("expected 3 service add calls, got %d", len(*calls))
	}
	want := [][]string{
		{"vault service add", "--name llm-gateway", "--host openrouter.ai", "--auth-type bearer", "--vault team-worker", "--token-key OR_KEY"},
		{"vault service add", "--name github", "--auth-type basic", "--username-key GIT_USER", "--password-key GH_TOKEN"},
		{"vault service add", "--name typefully", "--auth-type api-key", "--api-key-key TF_KEY", "--api-key-header X-API-KEY", "--api-key-prefix tok-"},
	}
	for i, w := range want {
		joined := strings.Join((*calls)[i].args, " ")
		for _, frag := range w {
			if !strings.Contains(joined, frag) {
				t.Errorf("service[%d] missing %q: %s", i, frag, joined)
			}
		}
		for _, e := range (*calls)[i].env {
			if strings.HasPrefix(e, "AGENT_VAULT_TOKEN=") {
				t.Errorf("service add must not set AGENT_VAULT_TOKEN, got %q", e)
			}
		}
	}
}
