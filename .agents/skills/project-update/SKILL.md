---
name: project-update
description: >-
  Runbook for updating Go dependencies, toolchains, and GitHub Actions in the
  fcitx5-gemini-asr repository. Use when the user asks to update project
  dependencies, upgrade Go or CI/tooling versions, or perform a routine
  dependency audit and bump.
---

# Project Update Runbook

Use this workflow when updating dependencies, Go toolchain/tooling versions, or
GitHub Actions in `fcitx5-gemini-asr` (`daemon/` Go module, `addon/` C++ Fcitx5
plugin, and `.github/` CI).

Never edit `daemon/go.sum` by hand.

## 1. Go Dependencies (`daemon/`)

1. **Check current & outdated direct dependencies**:
   ```bash
   (cd daemon && go list -u -m -f '{{if and (not .Indirect) .Update}}{{.Path}}: {{.Version}} -> {{.Update.Version}}{{end}}' all)
   ```
2. **Update Go dependencies**:
   - Routine minor/patch/compatible updates:
     ```bash
     just update-deps
     ```
     (`just update-deps` runs `cd daemon && go get -u -t ./... && go mod tidy`.)
   - Targeted or major version bumps: run `(cd daemon && go get <pkg>@<version>)`,
     then run `just tidy`.
3. **Check for Go vulnerabilities**:
   ```bash
   (cd daemon && go run golang.org/x/vuln/cmd/govulncheck@latest ./...)
   ```
4. **Check Go toolchain version**:
   - Check the `go X.Y.Z` directive in `daemon/go.mod`.
   - Keep the Go version aligned with `.github/workflows/ci.yml` (`env.GO_VERSION`).
   - Update `README.md` (`## Prerequisites`) if the minimum required Go version
     changes.

## 2. GitHub Actions & CI-Installed Dependencies

Inspect `.github/workflows/ci.yml` and `.github/dependabot.yml`.

1. **CI-installed tool & runtime versions** (`.github/workflows/ci.yml` `env:`):
   - `GO_VERSION`
   - `GOIMPORTS_VERSION` (`golang.org/x/tools/cmd/goimports@...`)
   - `GOLANGCI_LINT_VERSION` (verify compatibility with `.golangci.yml` when
     bumping major versions)
2. **GitHub Action versions** (`.github/workflows/ci.yml`):
   - `actions/checkout` (used in both `daemon` and `addon` jobs — update all
     occurrences together)
   - `actions/setup-go` (used in the `daemon` job)
   - Check latest action releases with `gh`:
     ```bash
     for repo in \
       actions/checkout \
       actions/setup-go
     do
       tag=$(gh api "repos/$repo/releases/latest" --jq '.tag_name')
       echo "$repo: $tag"
     done
     ```
   - Match the existing reference style in `.github/workflows/ci.yml` (major
     version tag such as `@v7`, or if pinned by commit SHA, update both the SHA
     and `# vX.Y.Z` comment together).
3. **C++ addon CI packages** (`.github/workflows/ci.yml` `addon` job):
   - `addon/` depends on system `apt` packages on `ubuntu-latest` (`cmake`,
     `g++`, `pkg-config`, `clang-format`, `libfcitx5core-dev`,
     `libfcitx5config-dev`, `libfcitx5utils-dev`) with no lockfile.
   - If `addon/CMakeLists.txt` or `README.md` (`## Prerequisites`) changes
     required build tools or libraries, keep the CI `apt-get install` list in
     sync.

## 3. Verification

Run the full local verification suite after all updates:

```bash
just check
```

(`just check` runs `fmt-check`, `lint`, `build`, and `test` across both `daemon/`
and `addon/`. If formatting changed, run `just fmt` first and re-run
`just check`.)

Check `git status` and `git diff` to verify only expected files changed, and
commit using conventional commits (e.g., `chore(deps): ...`, `build(deps): ...`,
or `ci: ...`).
