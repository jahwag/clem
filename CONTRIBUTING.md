# Contributing to Clementine

We're excited that you're interested in contributing to Clementine (`clem`)! This document outlines the process for contributing to this project.

## Getting Started

1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```
   git clone https://github.com/your-username/clem.git
   ```
3. Create a new branch for your feature or bug fix:
   ```
   git checkout -b feature/your-feature-name
   ```

## Setting Up the Development Environment

1. Ensure you have [Go](https://go.dev/dl/) 1.22 or later installed.
2. Fetch dependencies:
   ```
   go mod download
   ```
3. Build the binary:
   ```
   go build -o clem .
   ```

## Making Changes

1. Make your changes in your feature branch.
2. Add or update tests as necessary.
3. Run the tests to ensure they pass:
   ```
   go test ./...
   ```
4. Update the documentation if you've made changes to the CLI or `clem.yaml` schema.

## Code Style

We use the standard Go toolchain for formatting and linting. Run both locally before pushing:

```
go fmt ./...
go vet ./...
```

Use the `log` package (or stdlib equivalent) for diagnostic output — do **not** leave stray `fmt.Println` debug calls.

## Commit Messages — Conventional Commits

We follow the [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) specification. The commit message shape:

```
<type>(<optional scope>): <short summary>

<optional body>

<optional footer(s)>
```

**Types we use:**

| Type       | Use for                                                                        |
|------------|--------------------------------------------------------------------------------|
| `feat`     | A new feature for the user (e.g. a new `clem` subcommand, new config field)    |
| `fix`      | A bug fix                                                                      |
| `docs`     | README / CONTRIBUTING / inline doc changes only                                |
| `refactor` | Code change that neither fixes a bug nor adds a feature                        |
| `test`     | Adding or correcting tests                                                     |
| `perf`     | Performance improvement                                                        |
| `build`    | Changes to build system, Dockerfile, goreleaser                                |
| `ci`       | Changes to GitHub Actions workflows                                            |
| `chore`    | Maintenance tasks (dep bumps, linter config)                                   |
| `revert`   | Reverts a previous commit                                                      |

**Examples:**

```
feat(vault): warn when DISCORD_TOKEN starts with "Bot " prefix
fix(runner): auto-append kill $PPID when prompt is missing it
docs: document iteration duration format in README
ci(release): add SLSA v1.0 build provenance attestation
chore: bump github.com/spf13/cobra to v1.10.3
```

**Breaking changes** get a `!` after the type and a `BREAKING CHANGE:` footer:

```
feat(config)!: rename iteration_minutes to iteration (Go duration)

BREAKING CHANGE: iteration_minutes: int is replaced by iteration: string.
Existing configs must migrate — see README clem.yaml reference.
```

PRs with non-conforming commit messages will be asked to reword before merge.

## Signed Commits

All commits require **both**:

- `-s` — DCO sign-off, certifying you wrote or have the right to submit the code
- `-S` — cryptographic signature (GPG or SSH), producing a verified badge on GitHub

```
git commit -s -S -am "feat: add a brief description of your change"
```

Set up commit signing: https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits

Main branch has GitHub branch protection configured to **require signed commits**. Unverified commits cannot be merged.

## PR Checklist

Before submitting a pull request, confirm all of the following:

- [ ] Tests pass: `go test ./...`
- [ ] `go vet ./...` passes
- [ ] `go fmt ./...` produces no diff
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (`feat:`, `fix:`, `docs:`, …)
- [ ] All commits signed off and cryptographically signed (`git commit -s -S`)
- [ ] No hardcoded secrets, tokens, or personal identifiers
- [ ] README / docs updated if behaviour or schema changed

## Submitting Changes

1. Commit your changes (conventional format, signed + signed-off):
   ```
   git commit -s -S -am "feat: add a brief description of your change"
   ```
2. Push to your fork:
   ```
   git push origin feature/your-feature-name
   ```
3. Submit a pull request through the GitHub website.

## Reporting Bugs

If you find a bug, please open an issue on the GitHub repository. Include:

- A clear and concise description of the bug
- Steps to reproduce the behavior
- Expected behavior
- Any error messages or stack traces
- Your environment details (OS, Go version, `clem` version/commit)
- Relevant runner or watchdog logs (scrub any tokens before posting)

The more information you provide, the easier it will be for us to reproduce and fix the bug.

## Requesting Features

If you have an idea for a new feature, please open an issue on the GitHub repository. Describe the feature and why you think it would be useful for the project.

## Questions

If you have any questions about contributing, feel free to open an issue for discussion.

Thank you for your interest in improving Clementine!
