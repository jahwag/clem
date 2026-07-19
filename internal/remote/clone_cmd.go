package remote

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// remoteCloneCmd builds a remote git clone (or pull) command that authenticates
// private repos without embedding the OAuth token in the origin URL stored on disk.
// The token is passed only via a per-invocation http.extraHeader, which git does
// not persist into ~/.git/config.
func remoteCloneCmd(repoName, cleanURL, token string) string {
	repoDir := fmt.Sprintf("~/%s", repoName)
	if token == "" {
		return fmt.Sprintf("git clone %s %s 2>/dev/null || (cd %s && git pull)", cleanURL, repoDir, repoDir)
	}
	// GitHub's git-over-HTTPS endpoint rejects "Authorization: Bearer <token>"
	// for classic PATs ("invalid credentials"); it accepts HTTP Basic with the
	// token as the password. "x-access-token" as the username works for classic
	// PATs, fine-grained PATs, and App installation tokens alike.
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	header := "Authorization: Basic " + basic
	return fmt.Sprintf(
		`git -c http.extraheader=%s clone %s %s 2>/dev/null || (cd %s && git -c http.extraheader=%s pull)`,
		shellDoubleQuote(header),
		cleanURL,
		repoDir,
		repoDir,
		shellDoubleQuote(header),
	)
}

// shellDoubleQuote returns a double-quoted string safe for remote bash.
func shellDoubleQuote(s string) string {
	return `"` + strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `\$`,
		"`", "\\`",
	).Replace(s) + `"`
}
