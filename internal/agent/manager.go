package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jahwag/clem/internal/config"
)

// EnsureUser creates the OS user if it doesn't already exist.
func EnsureUser(username string) error {
	// Check if user exists
	cmd := exec.Command("id", username)
	if cmd.Run() == nil {
		fmt.Printf("  user %s already exists\n", username)
		return nil
	}
	fmt.Printf("  creating user %s\n", username)
	out, err := exec.Command("useradd",
		"--create-home",
		"--shell", "/bin/bash",
		"--comment", "clem managed agent",
		username,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd %s: %w\n%s", username, err, out)
	}
	return nil
}

// WriteEnvFile writes decrypted secrets to /home/<user>/.env with mode 0600.
func WriteEnvFile(username string, secrets map[string]string) error {
	homeDir := fmt.Sprintf("/home/%s", username)
	envPath := filepath.Join(homeDir, ".env")

	var sb strings.Builder
	for k, v := range secrets {
		sb.WriteString(fmt.Sprintf("export %s=%q\n", k, v))
	}

	if err := os.WriteFile(envPath, []byte(sb.String()), 0600); err != nil {
		return fmt.Errorf("writing .env for %s: %w", username, err)
	}

	// chown to the agent user
	out, err := exec.Command("chown", fmt.Sprintf("%s:%s", username, username), envPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown .env for %s: %w\n%s", username, err, out)
	}
	return nil
}

// InstallService writes and enables a systemd service for an agent.
func InstallService(cfg *config.Config, agentKey string, serviceContent string) error {
	serviceName := cfg.ServiceName(agentKey)
	servicePath := filepath.Join("/etc/systemd/system", serviceName)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing service file %s: %w", servicePath, err)
	}

	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}

	if out, err := exec.Command("systemctl", "enable", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// InstallServiceByName writes and enables a systemd service by explicit name.
func InstallServiceByName(serviceName string, serviceContent string) error {
	servicePath := filepath.Join("/etc/systemd/system", serviceName)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing service file %s: %w", servicePath, err)
	}

	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}

	if out, err := exec.Command("systemctl", "enable", serviceName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// InstallWatchdogTimer writes and enables the watchdog service + timer.
func InstallWatchdogTimer(cfg *config.Config, serviceContent, timerContent string) error {
	svcName := cfg.WatchdogServiceName()
	timerName := cfg.WatchdogTimerName()

	svcPath := filepath.Join("/etc/systemd/system", svcName)
	timerPath := filepath.Join("/etc/systemd/system", timerName)

	if err := os.WriteFile(svcPath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing watchdog service: %w", err)
	}
	if err := os.WriteFile(timerPath, []byte(timerContent), 0644); err != nil {
		return fmt.Errorf("writing watchdog timer: %w", err)
	}

	exec.Command("systemctl", "daemon-reload").Run()
	exec.Command("systemctl", "enable", "--now", timerName).Run()
	return nil
}

// StartService starts a systemd service.
func StartService(serviceName string) error {
	out, err := exec.Command("systemctl", "start", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl start %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// StopService stops a systemd service.
func StopService(serviceName string) error {
	out, err := exec.Command("systemctl", "stop", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// SystemdState returns the ActiveState of a systemd unit.
func SystemdState(serviceName string) string {
	out, err := exec.Command("systemctl", "show", "-p", "ActiveState", "--value", serviceName).Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// TmuxAlive returns true if a tmux session with the given name exists.
func TmuxAlive(sessionName string) bool {
	err := exec.Command("tmux", "has-session", "-t", sessionName).Run()
	return err == nil
}

// credentials is a subset of ~/.claude/.credentials.json
type credentials struct {
	ClaudeAiOauth struct {
		ExpiresAt int64 `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// TokenExpiry reads the Claude token expiry for a given OS user.
// Returns zero time if missing or unreadable.
func TokenExpiry(username string) time.Time {
	credPath := fmt.Sprintf("/home/%s/.claude/.credentials.json", username)
	data, err := os.ReadFile(credPath)
	if err != nil {
		return time.Time{}
	}
	var creds credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return time.Time{}
	}
	if creds.ClaudeAiOauth.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(creds.ClaudeAiOauth.ExpiresAt/1000, 0)
}

// NeedsLogin returns true if the token is missing or expires within 7 days.
func NeedsLogin(username string) bool {
	expiry := TokenExpiry(username)
	if expiry.IsZero() {
		return true
	}
	return time.Until(expiry) < 7*24*time.Hour
}

// ChownPath changes ownership of a path to the given user (best effort).
func ChownPath(path, username string) {
	exec.Command("chown", "-R", fmt.Sprintf("%s:%s", username, username), path).Run()
}

// LastLogLine returns the last non-empty line of a log file.
func LastLogLine(logPath string) string {
	out, err := exec.Command("tail", "-n", "1", logPath).Output()
	if err != nil {
		return "-"
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "-"
	}
	// truncate to 60 chars for table display
	if len(line) > 60 {
		return line[:57] + "..."
	}
	return line
}
