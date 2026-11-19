# Fcitx5 Gemini Voice IME (`fcitx5-gemini-asr`)

Real-time Linux voice input method built on **Fcitx5** and the **Gemini Live API**, featuring streaming inline preedit, a two-level Rime-style LLM candidate action menu with background prefetching, per-action model routing, and silent TPM2 credential storage.

## Architecture

- **Frontend (`addon/`)**: C++20 `fcitx::InputMethodEngineV2` plugin (`libgemini-voice.so`) communicating asynchronously over a Unix Domain Socket (`$XDG_RUNTIME_DIR/gemini-ime.sock`) using NDJSON and dispatching all UI updates to the Fcitx5 main loop via `EventDispatcher`.
- **Backend & CLI (`daemon/`)**: Single Go binary (`gemini-voice-daemon` with CLI alias `gemini-voice`) that captures 16 kHz mono s16le PCM via PipeWire (`pw-record --raw`), streams audio to Gemini Live (`BidiGenerateContent`), detects silence via RMS VAD, concurrently prefetches post-processing actions via `generateContent` (with per-action model overrides), hot-reloads `config.toml` via `fsnotify`, and manages interactive setup, model routing, and TPM2 credentials.
- **Systemd & Config (`systemd/`)**: User service unit (`LoadCredentialEncrypted` with TPM2) and TOML configuration template.

## Prerequisites

- **Fcitx5** development headers and CMake packages (`Fcitx5Core`, `Fcitx5Config`, `Fcitx5Utils`)
- **C++20** compiler (`g++` or `clang++`), `cmake`, `pkg-config`
- **Go** (>= 1.22)
- **PipeWire** (`pw-record`)
- **systemd** (`systemd-creds` with TPM2; falls back to `GEMINI_API_KEY` env var when `CREDENTIALS_DIRECTORY` is unset)
- Optional dev tooling: `just`, `golangci-lint`, `goimports`, `clang-format`

## Build & Install

```bash
# Build and run all Go and C++ tests without installing:
./install.sh --build-only

# Install system-wide addon (/usr/lib/fcitx5) + user daemon/CLI (~/.local/bin):
./install.sh --system

# Or install addon under ~/.local without sudo:
./install.sh --user
```

You can also use `just`:

```bash
just check         # fmt-check + lint + build + test
just install       # ./install.sh --system
just install-user  # ./install.sh --user
```

## CLI Setup, Models & Credential Management (`gemini-voice`)

The installer places `gemini-voice-daemon` and its CLI symlink `gemini-voice` in `~/.local/bin/`.

### Interactive Setup Wizard

```bash
# Full guided setup (input mode, language, VAD, live & per-action models, hotkey, proxy, and TPM2 API key):
gemini-voice init

# Initialize and immediately enable/start the systemd user service:
gemini-voice init --enable-service

# Re-run configuration wizard later without touching encrypted credentials:
gemini-voice configure
```

### Input Behavior, Models & Settings (`config` / `models`)

```bash
# Inspect all effective Fcitx5 addon and daemon settings:
gemini-voice config show

# Choose whether dictation opens the action menu or commits raw text directly:
gemini-voice config set ime.mode menu    # Show Refine / Translate / As Prompt menu (default)
gemini-voice config set ime.mode direct  # Commit raw transcript immediately

# Configure Fcitx5 trigger shortcut, language, VAD, prefetch, or proxy:
gemini-voice config set ime.trigger_key Alt+space
gemini-voice config set live.language zh-CN
gemini-voice config set live.vad_silence_ms 1200
gemini-voice config set post_process.prefetch refine_fix_typos,refine_formal
gemini-voice config set connection.proxy socks5://127.0.0.1:1080

# Inspect current model routing and optionally query available models from Gemini API:
gemini-voice models
gemini-voice models --remote

# Choose models for live ASR, default post-processing, and individual actions:
gemini-voice models --live gemini-3.5-transcribe-live --default gemini-2.5-flash --refine-formal gemini-2.5-pro --as-prompt gemini-2.5-pro
gemini-voice config set model.refine_fix_typos gemini-2.5-flash-lite
gemini-voice config set model.refine_formal default   # Clear override and inherit post_process.model

# Read a single setting or customize an action prompt:
gemini-voice config get ime.mode
gemini-voice config get model.as_prompt
gemini-voice config set prompt.translate_en "Translate cleanly to concise English:"
```

Daemon settings in `~/.config/gemini-voice/config.toml` are hot-reloaded automatically via `fsnotify`; Fcitx5 addon changes trigger `fcitx5-remote --check -r` when Fcitx5 is running.

### Credential Management (`auth`)

```bash
# Rotate or update the encrypted API key (interactive hidden prompt):
gemini-voice auth set --restart

# Non-interactive input (stdin or file):
gemini-voice auth set --key-file /path/to/key.txt
printf '%s' "$MY_KEY" | gemini-voice auth set --stdin

# If TPM2 is unavailable in a VM or container, fall back to auto or host key:
gemini-voice auth set --with-key auto

# Verify decryption (prints masked fingerprint only, never the raw key):
gemini-voice auth verify

# Inspect full system diagnostics (config, credential, TPM2, PipeWire, socket, systemd):
gemini-voice status

# Remove stored encrypted credential:
gemini-voice auth remove
```

For local development without `systemd-creds`:

```bash
GEMINI_API_KEY="your-key" ~/.local/bin/gemini-voice-daemon
```

## Usage & Hotkeys

1. Switch to **Gemini Voice** in Fcitx5 (`fcitx5-configtool`).
2. Press **`Alt+space`** (configurable via `gemini-voice config set ime.trigger_key ...` or `fcitx5-configtool`) to start recording. Live transcription streams at the cursor (`CapabilityFlag::Preedit`), automatically falling back to the candidate panel preedit on clients without inline preedit support.
3. Press **`Alt+space`** or **`Return`** (or pause for `vad_silence_ms`) to stop recording:
   - In **`menu`** mode (`ShowActionMenu = True`), the Rime-style action menu opens:
     - `1` / `Space` / `Return`: Commit raw transcript (`[Raw]`)
     - `2` / `Right`: Open **Refine** submenu (`1` Fix typos, `2` Casual tone, `3` Formal tone; `Left` / `Escape` / `BackSpace` returns to Level 1)
     - `3`: Translate to English (`translate_en`)
     - `4`: Convert to structured AI prompt (`as_prompt`)
     - `Escape`: Cancel and clear
   - In **`direct`** mode (`ShowActionMenu = False`), the raw transcript is committed immediately.

## Daemon Configuration (`~/.config/gemini-voice/config.toml`)

Prompts, live transcription model, default `post_process.model`, optional `[post_process.models]` per-action overrides, proxy (`http://`, `https://`, `socks5://`), language (`zh-CN`, `en-US`, `auto`), VAD silence timeout (`vad_silence_ms`), and `prefetch` actions live in `~/.config/gemini-voice/config.toml` and hot-reload automatically when saved.

## Development

Common recipes defined in `Justfile`:

- `just fmt` - format Go (`goimports`) and C++ (`clang-format`)
- `just fmt-check` - verify formatting without modifying files
- `just lint` - run `golangci-lint`, `bash -n install.sh`, and C++ `-Werror` build
- `just build` - build `daemon/gemini-voice-daemon`, `daemon/gemini-voice`, and `addon/build/libgemini-voice.so`
- `just test` - run `go test -v -race ./...` and `ctest`
- `just check` - run `fmt-check`, `lint`, `build`, and `test`

## Troubleshooting

- **Daemon connection error**: Run `gemini-voice status` or `systemctl --user status gemini-voice.service` and confirm `$XDG_RUNTIME_DIR/gemini-ime.sock` exists (`0600`).
- **Credential decryption failure**: Run `gemini-voice auth verify`; if `CREDENTIALS_DIRECTORY` is set by systemd, the encrypted credential file must exist and match the active TPM2/host scope.
- **No audio captured**: Verify `pw-record --raw --rate=16000 --channels=1 --format=s16 - > /dev/null` works with your PipeWire microphone source.
