package runner

// GenerateCodexUpdater returns the agent-owned updater used both while
// provisioning and by every Codex runner iteration. Releases are immutable;
// the public executable is a symlink changed with a same-directory rename.
func GenerateCodexUpdater() string { return codexUpdaterScript }

const codexUpdaterScript = `#!/bin/bash
set -u

ROOT="$HOME/.npm-global/codex"
RELEASES="$ROOT/releases"
STATE="$ROOT/state"
BIN_DIR="$HOME/.npm-global/bin"
LIVE="$BIN_DIR/codex"
COOLDOWN=3600
CHECK_INTERVAL=86400
SOAK_SECONDS=86400

mkdir -p "$RELEASES" "$STATE/quarantine" "$BIN_DIR"

say() { echo "codex-updater: $*"; }
version_of() {
    [ -x "$1" ] || return 1
    "$1" --version 2>/dev/null | sed -n 's/^codex-cli[[:space:]]\+//p' | head -1
}
validate() {
    local bin="$1" version="$2" help
    [ -x "$bin" ] || { say "candidate $version has no executable"; return 1; }
    [ "$(version_of "$bin")" = "$version" ] || {
        say "candidate $version failed version validation"; return 1;
    }
    help=$("$bin" --help 2>&1) || { say "candidate $version failed help validation"; return 1; }
    for arg in --dangerously-bypass-approvals-and-sandbox --model --cd; do
        grep -q -- "$arg" <<<"$help" || { say "candidate $version lacks required argument $arg"; return 1; }
    done
}
atomic_link() {
    local target="$1" link="$2" tmp
    tmp="${link}.new.$$"
    ln -s "$target" "$tmp" && mv -Tf "$tmp" "$link"
}
current_target() { readlink -f "$LIVE" 2>/dev/null || true; }
current_version() { version_of "$LIVE" 2>/dev/null || true; }

resolve_soaked_version() {
    local metadata
    metadata=$(npm view @openai/codex time --json 2>/dev/null) || return 1
    printf '%s' "$metadata" | node -e '
const fs = require("fs");
const times = JSON.parse(fs.readFileSync(0, "utf8"));
const cutoff = Date.now() - Number(process.argv[1]) * 1000;
const versions = Object.entries(times)
  .filter(([v, published]) => /^\d+\.\d+\.\d+$/.test(v) && Date.parse(published) <= cutoff)
  .map(([v]) => v)
  .sort((a, b) => {
    const aa = a.split(".").map(Number), bb = b.split(".").map(Number);
    return aa[0] - bb[0] || aa[1] - bb[1] || aa[2] - bb[2];
  });
if (!versions.length) process.exit(1);
process.stdout.write(versions[versions.length - 1]);
' "$SOAK_SECONDS"
}

rollback_early() {
    local bad="$1" previous previous_version
    [ "$(cat "$STATE/promoted" 2>/dev/null)" = "$bad" ] || return 0
    previous=$(readlink -f "$STATE/previous" 2>/dev/null || true)
    previous_version=$(version_of "$previous" 2>/dev/null || true)
    if [ -n "$previous_version" ] && validate "$previous" "$previous_version"; then
        atomic_link "$previous" "$LIVE" || return 1
        : > "$STATE/quarantine/$bad"
        rm -f "$STATE/promoted"
        say "rolled back early startup failure for $bad; version quarantined"
        return 0
    fi
    say "cannot roll back $bad: no validated previous release"
    return 1
}

update() {
    command -v npm >/dev/null 2>&1 || { say "npm not found"; return 1; }
    command -v node >/dev/null 2>&1 || { say "node not found"; return 1; }
    local version active candidate cache stamp now old old_version target resolve_stamp check_stamp live_target managed
    resolve_stamp="$STATE/resolve-failed"
    check_stamp="$STATE/last-check"
    now=$(date +%s)
    active=$(current_version)
    live_target=$(current_target)
    managed=false
    case "$live_target" in "$RELEASES"/*) managed=true ;; esac
    # A healthy installation checks for an eligible release once per day. A
    # missing, broken, or legacy in-place installation bypasses this gate so
    # provisioning, migration, and outage recovery never wait for the next
    # scheduled check.
    if $managed && [ -n "$active" ] && validate "$LIVE" "$active" >/dev/null 2>&1 &&
       [ -f "$check_stamp" ] &&
       [ $((now - $(stat -c %Y "$check_stamp" 2>/dev/null || echo 0))) -lt $CHECK_INTERVAL ]; then
        return 0
    fi
    if [ -f "$resolve_stamp" ] && [ $((now - $(stat -c %Y "$resolve_stamp" 2>/dev/null || echo 0))) -lt $COOLDOWN ]; then
        return 1
    fi
    version=$(resolve_soaked_version) || {
        say "could not resolve a stable Codex release older than 24h; retrying after cooldown"
        touch "$resolve_stamp"; return 1
    }
    touch "$check_stamp"
    rm -f "$resolve_stamp"
    version=${version//$'\r'/}
    [ -n "$version" ] || { say "npm returned an empty stable version"; return 1; }
    $managed && [ "$active" = "$version" ] && return 0
    [ -e "$STATE/quarantine/$version" ] && { say "stable $version is quarantined; keeping ${active:-none}"; return 0; }

    stamp="$STATE/failed-$version"
    if [ -f "$stamp" ] && [ $((now - $(stat -c %Y "$stamp" 2>/dev/null || echo 0))) -lt $COOLDOWN ]; then
        return 0
    fi

    target="$RELEASES/$version/node_modules/.bin/codex"
    if ! validate "$target" "$version" >/dev/null 2>&1; then
        candidate=$(mktemp -d "$ROOT/candidate-$version.XXXXXX") || return 1
        cache=$(mktemp -d "$ROOT/cache-$version.XXXXXX") || { rm -rf "$candidate"; return 1; }
        if ! npm install --prefix "$candidate" --cache "$cache" --include=optional --no-audit --no-fund "@openai/codex@$version"; then
            say "download failed for $version; keeping ${active:-none}"
            touch "$stamp"; rm -rf "$candidate" "$cache"; return 1
        fi
        rm -rf "$cache"
        if ! validate "$candidate/node_modules/.bin/codex" "$version"; then
            say "validation failed for $version; keeping ${active:-none}"
            touch "$stamp"; rm -rf "$candidate"; return 1
        fi
        rm -rf "$RELEASES/$version"
        mv "$candidate" "$RELEASES/$version" || return 1
    fi

    old=$(current_target)
    old_version=$(version_of "$old" 2>/dev/null || true)
    if [ -n "$old_version" ] && validate "$old" "$old_version" >/dev/null 2>&1; then
        atomic_link "$old" "$STATE/previous"
    fi
    atomic_link "$target" "$LIVE" || return 1
    echo "$version" > "$STATE/promoted"
    rm -f "$stamp"
    say "promoted codex $version (previous ${active:-none})"
}

case "${1:-update}" in
    update) update ;;
    require) update && validate "$LIVE" "$(current_version)" ;;
    rollback-early) [ -n "${2:-}" ] || exit 2; rollback_early "$2" ;;
    clear-promotion) rm -f "$STATE/promoted" ;;
    *) say "usage: $0 [update|require|rollback-early VERSION|clear-promotion]"; exit 2 ;;
esac
`
