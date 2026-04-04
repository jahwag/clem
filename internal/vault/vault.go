package vault

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultAgeKeysPath = ".config/sops/age/keys.txt"
const secretsFile = "secrets.sops.yaml"

// Init generates an age keypair and saves it to ~/.config/sops/age/keys.txt.
// Prints the public key and instructions for .sops.yaml.
func Init() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}

	keysPath := filepath.Join(home, defaultAgeKeysPath)
	if err := os.MkdirAll(filepath.Dir(keysPath), 0700); err != nil {
		return fmt.Errorf("creating keys dir: %w", err)
	}

	// Check if age-keygen is available
	if _, err := exec.LookPath("age-keygen"); err != nil {
		return fmt.Errorf("age-keygen not found — install age: https://github.com/FiloSottile/age")
	}

	out, err := exec.Command("age-keygen", "-o", keysPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("age-keygen: %w\n%s", err, out)
	}

	// Extract public key from the generated file
	data, err := os.ReadFile(keysPath)
	if err != nil {
		return fmt.Errorf("reading keys file: %w", err)
	}

	pubKey := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# public key:") {
			pubKey = strings.TrimSpace(strings.TrimPrefix(line, "# public key:"))
			break
		}
	}

	fmt.Printf("Age keypair generated at: %s\n", keysPath)
	fmt.Printf("Public key: %s\n", pubKey)

	// Write .sops.yaml if it doesn't already exist
	const sopsCfgFile = ".sops.yaml"
	if _, err := os.Stat(sopsCfgFile); os.IsNotExist(err) {
		content := fmt.Sprintf("creation_rules:\n  - path_regex: secrets\\.sops\\.yaml\n    age: %s\n", pubKey)
		if err := os.WriteFile(sopsCfgFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing .sops.yaml: %w", err)
		}
		fmt.Printf("Wrote %s — commit this file to your repo.\n", sopsCfgFile)
	} else {
		fmt.Printf("%s already exists — add the public key manually if needed.\n", sopsCfgFile)
	}

	fmt.Println("\nBack up your private key:")
	fmt.Printf("  cat %s\n", keysPath)
	return nil
}

// Set sets a secret key for an agent in secrets.sops.yaml using sops --set.
// keyval should be "KEY=value".
func Set(agentKey, keyval string) error {
	parts := strings.SplitN(keyval, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format, expected KEY=value, got: %s", keyval)
	}
	key, value := parts[0], parts[1]

	if err := ensureSops(); err != nil {
		return err
	}

	// sops --set '["agents"]["<agentKey>"]["KEY"] "value"' secrets.sops.yaml
	setExpr := fmt.Sprintf(`["agents"]["%s"]["%s"] "%s"`, agentKey, key, value)
	out, err := exec.Command("sops", "--set", setExpr, secretsFile).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sops --set: %w\n%s", err, out)
	}
	fmt.Printf("Set %s.%s\n", agentKey, key)
	return nil
}

// Get retrieves a secret key for an agent from secrets.sops.yaml.
func Get(agentKey, key string) error {
	if err := ensureSops(); err != nil {
		return err
	}

	// Decrypt and extract with yq
	decrypted, err := sopsDecrypt()
	if err != nil {
		return err
	}

	yqExpr := fmt.Sprintf(".agents.%s.%s", agentKey, key)
	out, err := runYQ(yqExpr, decrypted)
	if err != nil {
		return fmt.Errorf("yq: %w", err)
	}
	fmt.Println(strings.TrimSpace(out))
	return nil
}

// List prints all secrets (keys only, not values) from secrets.sops.yaml.
func List() error {
	if err := ensureSops(); err != nil {
		return err
	}

	decrypted, err := sopsDecrypt()
	if err != nil {
		return err
	}

	out, err := runYQ(".agents | keys | .[]", decrypted)
	if err != nil {
		return fmt.Errorf("yq: %w", err)
	}

	fmt.Println("Agents with secrets:")
	for _, agent := range strings.Split(strings.TrimSpace(out), "\n") {
		if agent == "" {
			continue
		}
		fmt.Printf("  %s:\n", agent)
		keysOut, err := runYQ(fmt.Sprintf(".agents.%s | keys | .[]", agent), decrypted)
		if err != nil {
			continue
		}
		for _, k := range strings.Split(strings.TrimSpace(keysOut), "\n") {
			if k != "" {
				fmt.Printf("    - %s\n", k)
			}
		}
	}
	return nil
}

// DecryptAgent returns the decrypted secrets for a specific agent as a map.
func DecryptAgent(agentKey string) (map[string]string, error) {
	if err := ensureSops(); err != nil {
		return nil, err
	}

	decrypted, err := sopsDecrypt()
	if err != nil {
		return nil, err
	}

	// Use yq to get key=value pairs
	yqExpr := fmt.Sprintf(".agents.%s | to_entries | .[] | .key + \"=\" + .value", agentKey)
	out, err := runYQ(yqExpr, decrypted)
	if err != nil {
		return nil, fmt.Errorf("yq: %w", err)
	}

	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result, nil
}

func sopsDecrypt() (string, error) {
	out, err := exec.Command("sops", "--decrypt", secretsFile).Output()
	if err != nil {
		return "", fmt.Errorf("sops --decrypt: %w", err)
	}
	return string(out), nil
}

func runYQ(expr, input string) (string, error) {
	if _, err := exec.LookPath("yq"); err != nil {
		return "", fmt.Errorf("yq not found — install yq: https://github.com/mikefarah/yq")
	}
	cmd := exec.Command("yq", "e", expr, "-")
	cmd.Stdin = bytes.NewBufferString(input)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ensureSops() error {
	if _, err := exec.LookPath("sops"); err != nil {
		return fmt.Errorf("sops not found — install sops: https://github.com/getsops/sops")
	}
	if _, err := os.Stat(secretsFile); os.IsNotExist(err) {
		return fmt.Errorf("%s not found in current directory", secretsFile)
	}
	return nil
}
