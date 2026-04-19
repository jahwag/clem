package agent

import (
	"strings"
	"testing"
)

// The pre-push hook is the client-side backstop against secret leaks. If the
// pattern set regresses, pushes that should be blocked will go through. Lock
// the critical patterns in here so a future refactor has to think about it.
func TestPrePushHookContent_ContainsCriticalSecretPatterns(t *testing.T) {
	required := []string{
		"ghp_[A-Za-z0-9]{36}",                // GitHub classic PAT
		"github_pat_",                        // GitHub fine-grained PAT
		"gho_",                               // GitHub OAuth app token
		"ghs_",                               // GitHub App server-to-server
		"sk-",                                // OpenAI / Anthropic style
		"xox[bapr]-",                         // Slack bot / user / refresh
		"AKIA",                               // AWS access key
		"AGE-SECRET-KEY-1",                   // age encryption private key
		"BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY", // SSH / TLS keys
	}
	for _, pat := range required {
		if !strings.Contains(prePushHookContent, pat) {
			t.Errorf("pre-push hook missing pattern %q", pat)
		}
	}
}

func TestPrePushHookContent_IsExecutableBash(t *testing.T) {
	if !strings.HasPrefix(prePushHookContent, "#!/bin/bash") {
		t.Error("pre-push hook should start with a bash shebang")
	}
	if !strings.Contains(prePushHookContent, "exit 1") {
		t.Error("pre-push hook should exit 1 on secret match (blocks push)")
	}
	if !strings.Contains(prePushHookContent, "exit 0") {
		t.Error("pre-push hook should exit 0 on clean push")
	}
}
