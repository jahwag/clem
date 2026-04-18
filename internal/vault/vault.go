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

// Set sets a secret key for a vault in secrets.sops.yaml using sops --set.
// keyval should be "KEY=value".
func Set(vaultName, keyval string) error {
	parts := strings.SplitN(keyval, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid format, expected KEY=value, got: %s", keyval)
	}
	key, value := parts[0], parts[1]

	if err := ensureSopsBin(); err != nil {
		return err
	}

	// First-use bootstrap: sops --set refuses to create the file. Write a
	// minimal plaintext stub and encrypt it in place, relying on .sops.yaml
	// creation_rules for the age recipient.
	if _, err := os.Stat(secretsFile); os.IsNotExist(err) {
		if err := os.WriteFile(secretsFile, []byte("vaults: {}\n"), 0644); err != nil {
			return fmt.Errorf("creating %s: %w", secretsFile, err)
		}
		out, err := exec.Command("sops", "-e", "-i", secretsFile).CombinedOutput()
		if err != nil {
			_ = os.Remove(secretsFile)
			return fmt.Errorf("sops encrypt (first init): %w\n%s", err, out)
		}
	}

	// sops --set '["vaults"]["<vaultName>"]["KEY"] "value"' secrets.sops.yaml
	setExpr := fmt.Sprintf(`["vaults"]["%s"]["%s"] "%s"`,
		strings.ReplaceAll(vaultName, `"`, `\"`),
		strings.ReplaceAll(key, `"`, `\"`),
		strings.ReplaceAll(value, `"`, `\"`),
	)
	out, err := exec.Command("sops", "--set", setExpr, secretsFile).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sops --set: %w\n%s", err, out)
	}
	fmt.Printf("Set %s.%s\n", vaultName, key)
	return nil
}

// Delete removes a secret key (or whole vault if key is empty) from secrets.sops.yaml.
func Delete(vaultName, key string) error {
	if err := ensureSops(); err != nil {
		return err
	}

	var unsetExpr string
	if key == "" {
		unsetExpr = fmt.Sprintf(`["vaults"]["%s"]`, strings.ReplaceAll(vaultName, `"`, `\"`))
	} else {
		unsetExpr = fmt.Sprintf(`["vaults"]["%s"]["%s"]`,
			strings.ReplaceAll(vaultName, `"`, `\"`),
			strings.ReplaceAll(key, `"`, `\"`),
		)
	}

	// sops unset takes "file index" — file before index
	out, err := exec.Command("sops", "unset", secretsFile, unsetExpr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sops unset: %w\n%s", err, out)
	}
	if key == "" {
		fmt.Printf("Deleted vault %s\n", vaultName)
	} else {
		fmt.Printf("Deleted %s.%s\n", vaultName, key)
	}
	return nil
}

// Get retrieves a secret key for a vault from secrets.sops.yaml.
func Get(vaultName, key string) error {
	if err := ensureSops(); err != nil {
		return err
	}

	decrypted, err := sopsDecrypt()
	if err != nil {
		return err
	}

	yqExpr := fmt.Sprintf(".vaults.%s.%s", vaultName, key)
	out, err := runYQ(yqExpr, decrypted)
	if err != nil {
		return fmt.Errorf("yq: %w", err)
	}
	fmt.Println(strings.TrimSpace(out))
	return nil
}

// List prints all vaults and their keys (not values) from secrets.sops.yaml.
func List() error {
	if err := ensureSops(); err != nil {
		return err
	}

	decrypted, err := sopsDecrypt()
	if err != nil {
		return err
	}

	// Detect legacy structure
	hasVaults, err := yamlKeyExists(".vaults", decrypted)
	if err != nil {
		return err
	}
	hasAgents, err := yamlKeyExists(".agents", decrypted)
	if err != nil {
		return err
	}

	if !hasVaults && hasAgents {
		fmt.Fprintln(os.Stderr, "warning: secrets.sops.yaml uses legacy agents: structure — migrate to vaults: for shared secrets")
		return listLegacy(decrypted)
	}

	out, err := runYQ(".vaults | keys | .[]", decrypted)
	if err != nil {
		return fmt.Errorf("yq: %w", err)
	}

	fmt.Println("Vaults:")
	for _, vault := range strings.Split(strings.TrimSpace(out), "\n") {
		if vault == "" {
			continue
		}
		fmt.Printf("  %s:\n", vault)
		keysOut, err := runYQ(fmt.Sprintf(".vaults.%s | keys | .[]", vault), decrypted)
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

// DecryptForAgent returns the merged secrets for an agent by merging the named vaults in order.
// Later vaults in the list win on key conflicts.
// Falls back to legacy agents: structure with a warning if vaults: key is absent.
func DecryptForAgent(agentKey string, vaultNames []string) (map[string]string, error) {
	if err := ensureSops(); err != nil {
		return nil, err
	}

	decrypted, err := sopsDecrypt()
	if err != nil {
		return nil, err
	}

	// Detect legacy structure
	hasVaults, err := yamlKeyExists(".vaults", decrypted)
	if err != nil {
		return nil, err
	}
	hasAgents, err := yamlKeyExists(".agents", decrypted)
	if err != nil {
		return nil, err
	}

	if !hasVaults && hasAgents {
		fmt.Fprintf(os.Stderr, "warning: secrets.sops.yaml uses legacy agents: structure — migrate to vaults: for shared secrets\n")
		return decryptLegacyAgent(agentKey, decrypted)
	}

	if len(vaultNames) == 0 {
		return map[string]string{}, nil
	}

	result := make(map[string]string)
	for _, vaultName := range vaultNames {
		yqExpr := fmt.Sprintf(".vaults.%s | to_entries | .[] | .key + \"=\" + .value", vaultName)
		out, err := runYQ(yqExpr, decrypted)
		if err != nil {
			return nil, fmt.Errorf("yq for vault %s: %w", vaultName, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				result[parts[0]] = parts[1]
			}
		}
	}
	return result, nil
}

func decryptLegacyAgent(agentKey, decrypted string) (map[string]string, error) {
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

func listLegacy(decrypted string) error {
	out, err := runYQ(".agents | keys | .[]", decrypted)
	if err != nil {
		return fmt.Errorf("yq: %w", err)
	}

	fmt.Println("Agents with secrets (legacy structure):")
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

func yamlKeyExists(key, input string) (bool, error) {
	out, err := runYQ(fmt.Sprintf("%s | type", key), input)
	if err != nil {
		return false, nil // yq returns error if key missing
	}
	t := strings.TrimSpace(out)
	return t != "null" && t != "!!null", nil
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
		return fmt.Errorf("%s not found — run 'clem vault set' to create it", secretsFile)
	}
	return nil
}

// ensureSopsBin checks only that the sops binary exists, not the secrets file.
// Used by Set which creates the file on first use.
func ensureSopsBin() error {
	if _, err := exec.LookPath("sops"); err != nil {
		return fmt.Errorf("sops not found — install sops: https://github.com/getsops/sops")
	}
	return nil
}
