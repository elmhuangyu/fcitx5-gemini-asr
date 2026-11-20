#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_ONLY=false

usage() {
    cat <<'EOF'
Usage: ./install.sh [--build-only]

Default behavior installs the Fcitx5 addon to Fcitx5's default prefix (via sudo
if needed), installs the Go daemon/CLI to ~/.local/bin, and sets up systemd
user files.

Options:
  --build-only  Compile C++ addon & Go daemon/CLI and run all test suites without
                modifying ~/.local or ~/.config.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --build-only)
            BUILD_ONLY=true
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

for cmd in cmake go pkg-config; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "Missing required tool: $cmd" >&2
        exit 1
    fi
done

BUILD_VERSION="$(date -u +%Y-%m-%d-%H-%M-%S):$(git -C "${ROOT_DIR}" rev-parse --short=8 HEAD 2>/dev/null || echo nogit)"

echo "==> [1/4] Building & testing Go daemon and CLI..."
(
    cd "${ROOT_DIR}/daemon"
    go vet ./...
    go test -race ./...
    go build -ldflags "-X main.Version=${BUILD_VERSION}" -o "${ROOT_DIR}/daemon/gemini-voice-daemon" .
    ln -sfn gemini-voice-daemon "${ROOT_DIR}/daemon/gemini-voice"
)

CMAKE_PREFIX="$(pkg-config --variable=prefix Fcitx5Core 2>/dev/null || echo /usr)"

echo "==> [2/4] Building & testing Fcitx5 C++ addon (prefix: ${CMAKE_PREFIX})..."
cmake -S "${ROOT_DIR}/addon" -B "${ROOT_DIR}/addon/build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX="${CMAKE_PREFIX}" \
    -DENABLE_TEST=ON
cmake --build "${ROOT_DIR}/addon/build" -j"$(nproc)"
ctest --test-dir "${ROOT_DIR}/addon/build" --output-on-failure

if [[ "${BUILD_ONLY}" == "true" ]]; then
    echo "==> Build & tests succeeded (--build-only: skipped installation)."
    exit 0
fi

echo "==> [3/4] Installing Fcitx5 addon (prefix: ${CMAKE_PREFIX})..."
if [[ -w "${CMAKE_PREFIX}" ]]; then
    cmake --install "${ROOT_DIR}/addon/build"
else
    sudo cmake --install "${ROOT_DIR}/addon/build"
fi
rm -f "$HOME/.local/lib/fcitx5/libgemini-voice.so" \
      "$HOME/.local/share/fcitx5/addon/gemini-voice.conf" \
      "$HOME/.local/share/locale/zh_CN/LC_MESSAGES/fcitx5-gemini-voice.mo"

echo "==> [4/4] Installing Go daemon/CLI, config, and systemd user service..."
mkdir -p "$HOME/.local/bin" "$HOME/.config/gemini-voice" "$HOME/.config/systemd/user"
install -m 0755 "${ROOT_DIR}/daemon/gemini-voice-daemon" "$HOME/.local/bin/gemini-voice-daemon"
ln -sfn gemini-voice-daemon "$HOME/.local/bin/gemini-voice"

if [[ ! -f "$HOME/.config/gemini-voice/config.toml" ]]; then
    install -m 0600 "${ROOT_DIR}/systemd/config.toml" "$HOME/.config/gemini-voice/config.toml"
    echo "Installed default config to $HOME/.config/gemini-voice/config.toml"
else
    echo "Keeping existing config at $HOME/.config/gemini-voice/config.toml"
fi

install -m 0644 "${ROOT_DIR}/systemd/gemini-voice.service" "$HOME/.config/systemd/user/gemini-voice.service"

if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'

Installation complete! Next steps:

1. Initialize configuration and encrypt your Gemini API key:
   gemini-voice init
   (Or update only the key later with: gemini-voice auth set)

2. Enable and start the background daemon:
   systemctl --user enable --now gemini-voice.service

3. Restart Fcitx5 (e.g. `fcitx5 -r -d`) and enable "Gemini Voice" in fcitx5-configtool.
EOF
