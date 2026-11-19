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
   - `FCITX5_VERSION` (fcitx5 tag the `addon` job builds from source; see
     "C++ addon CI toolchain" below for the bump procedure)
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
3. **C++ addon CI toolchain** (`.github/workflows/ci.yml` `addon` job):
   - `ubuntu-latest` (26.04) ships fcitx5 5.1.19 in apt, which is older than
     the APIs the addon and its integration tests require
     (`Instance::showCustomInputMethodInformation` needs 5.1.11;
     `InputPanel::overlayMessage` in `addon/tests/` needs 5.1.22). CI must
     therefore **not** install the distro `libfcitx5*-dev` packages.
   - Instead, the "Build and install fcitx5 core" step clones
     `fcitx/fcitx5` at `env.FCITX5_VERSION` and builds it from source as an
     embedded library (all optional features off: `ENABLE_SERVER`, `ENABLE_DBUS`,
     `ENABLE_X11`, `ENABLE_WAYLAND`, `ENABLE_KEYBOARD`, `ENABLE_ENCHANT`,
     `ENABLE_EMOJI`, `BUILD_SPELL_DICT` — see the workflow for the full flag
     set), then installs to `/usr` and runs `sudo ldconfig`.
   - The remaining `apt` packages for the job (`cmake`, `g++`, `make`,
     `pkg-config`, `clang-format`, `gettext`, `extra-cmake-modules`,
     `libuv1-dev`, `uuid-dev`, `git`) cover the toolchain plus fcitx5's own
     build dependencies (event loop via libuv, ECM, uuid); there is no
     lockfile.
   - Bumping fcitx5:
     ```bash
     gh api repos/fcitx/fcitx5/releases/latest --jq .tag_name
     ```
     Update `FCITX5_VERSION` in `.github/workflows/ci.yml` `env:`. Keep it
     >= the minimum version the addon builds against (currently 5.1.22) and
     in sync with `README.md` (`## Prerequisites`).
   - If `addon/CMakeLists.txt` or `README.md` (`## Prerequisites`) changes
     required build tools or libraries, keep both the CI `apt-get install`
     list and the fcitx5 source-build flags in sync.

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
