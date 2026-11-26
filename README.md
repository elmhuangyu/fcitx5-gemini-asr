# Fcitx5 Gemini Voice IME (`fcitx5-gemini-asr`)

[![CI](https://github.com/elmhuangyu/fcitx5-gemini-asr/actions/workflows/ci.yml/badge.svg)](https://github.com/elmhuangyu/fcitx5-gemini-asr/actions/workflows/ci.yml)

Real-time Linux voice input method built on **Fcitx5** and the **Gemini Live API**, featuring streaming inline preedit, a two-level Rime-style LLM candidate action menu with background prefetching, per-action model routing, and silent `systemd-creds` user credential storage.

## Architecture

- **Frontend (`addon/`)**: C++20 `fcitx::InputMethodEngineV2` plugin (`libgemini-voice.so`) communicating asynchronously over a Unix Domain Socket (`$XDG_RUNTIME_DIR/gemini-ime.sock`) using NDJSON and dispatching all UI updates to the Fcitx5 main loop via `EventDispatcher`.
- **Backend & CLI (`daemon/`)**: Single Go binary (`gemini-voice-daemon` with CLI alias `gemini-voice`) that captures 16 kHz mono s16le PCM via PipeWire (`pw-record --raw`), streams audio over the Gemini Live API (`BidiGenerateContent`, defaulting to `gemini-3.5-transcribe-live` and configurable via `live_transcribe.model` / `gemini-voice models --live`), relies on the Live API's server-side voice activity detection (`automaticActivityDetection`) to auto-stop on silence, uses the official `google.golang.org/genai` SDK, concurrently prefetches post-processing actions via `generateContent` (with per-action model overrides), hot-reloads `config.toml` via `fsnotify`, and provides an interactive TUI setup form, model routing, and `systemd-creds` credential management.
- **Systemd & Config (`systemd/`)**: User service unit (`LoadCredentialEncrypted`) and TOML configuration template.

## Prerequisites

- **Fcitx5** (>= 5.1.22) development headers and CMake packages (`Fcitx5Core`, `Fcitx5Config`, `Fcitx5Utils`; note that Ubuntu 26.04's `libfcitx5*-dev` is 5.1.19 and too old — build fcitx5 from source or use a newer distro, matching `.github/workflows/ci.yml` `FCITX5_VERSION`)
- **C++20** compiler (`g++` or `clang++`), `cmake`, `pkg-config`
- **Network access at configure time**: CMake downloads nlohmann/json (pinned v3.12.0, SHA256-verified) via FetchContent; no system package is needed
- **Go** (>= 1.27.4, matching `daemon/go.mod`)
- **PipeWire** (`pw-record`)
- **systemd** (`systemd-creds --user`; falls back to `GEMINI_API_KEY` env var when `CREDENTIALS_DIRECTORY` is unset)
- Optional build & dev tooling: `gettext` (`msgfmt`, for compiling UI translations in `addon/`), `just`, `golangci-lint`, `goimports`, `clang-format`

## Build & Install

```bash
# Build and run all Go and C++ tests without installing:
./install.sh --build-only

# Install Fcitx5 addon to Fcitx5's default prefix (via sudo if needed) + daemon/CLI (~/.local/bin):
./install.sh
```

You can also use `just`:

```bash
just check           # fmt-check + lint + build + test
just install         # ./install.sh (addon to Fcitx5 prefix, daemon/CLI to ~/.local/bin)
```

## CLI Setup, Models & Credential Management (`gemini-voice`)

The installer places `gemini-voice-daemon` and its CLI symlink `gemini-voice` in `~/.local/bin/`.

### Interactive TUI Setup (`init` / `configure`)

Both `gemini-voice init` and `gemini-voice configure` launch an interactive multi-step TUI form (`charm.land/huh/v2`):

- **Form steps**:
  1. **Input & Recognition**: action menu vs. direct commit, transcription language (`auto` or comma-separated BCP-47 tags), `VERBATIM`/`SMART` mode, and VAD silence timeout
  2. **Models**: live transcription model, default post-processing model, and whether to configure per-action overrides
  3. **Per-Action Model Overrides** *(shown when enabled)*: model overrides for `refine_fix_typos`, `refine_casual`, `refine_formal`, `as_prompt`, dynamic `translate_<lang>` targets (e.g., `translate_en`), and `ask_ai`
  4. **Action Menu Prefetch** *(shown in `menu` mode)*: multi-select actions to prefetch when recording stops
  5. **Vocabulary, Hotkey & Network**: custom vocabulary (`custom_vocabulary`), Fcitx5 trigger key(s), and HTTP/SOCKS5 proxy
  6. **Gemini API Key** *(`init` only)*: masked password input (`systemd-creds` encryption); if a `.cred` file already exists, prompts whether to keep or overwrite it
- **TUI keybindings**: `↑`/`↓` (`Shift+Tab`/`Tab`/`Enter`) move between options and fields, `←`/`→` edit text cursors or toggle Yes/No confirmations, `Space`/`x` toggle multi-select checkboxes, `Ctrl+E` accepts autocomplete suggestions, and `Ctrl+C` cancels without saving any changes.

```bash
# Full interactive TUI setup (config, Fcitx5 addon settings, and encrypted API key):
gemini-voice init

# Initialize and immediately enable/start the systemd user service:
gemini-voice init --enable-service

# Explicitly select the host key for user-scoped encryption (default is auto):
gemini-voice init --with-key host

# Scripted / non-interactive initialization (skips TUI, writes default config.toml if missing):
printf '%s' "$MY_KEY" | gemini-voice init --stdin --non-interactive
gemini-voice init --key-file /path/to/key.txt

# Re-run the interactive TUI form later without touching encrypted credentials (requires a TTY; use `config set` in scripts):
gemini-voice configure
```

### Input Behavior, Models & Settings (`config` / `models`)

**Thinking effort.** Any post-processing model string (`post_process.model`, `[post_process.models]`, `models --default/--refine-*/--translate`, `config set model.<action>`, TUI) accepts a `/minimal`, `/low`, `/medium` or `/high` suffix, e.g. `gemini-3.5-flash/low`. The suffix is stripped from the API URL and sent as `thinkingConfig.thinkingLevel` (`gemini-3*` models only; `minimal` is raised to `low` on Pro and `gemini-3.8-flash`, and `medium` to `high` on `gemini-3-pro`). Other model families reject a suffix; unknown suffixes are rejected. Without a suffix, `refine_*` and `translate_*` default to `minimal`; `ask_ai` and `as_prompt` use the provider default. Suffixes are not valid for `live_transcribe.model`.

**Prompt isolation.** The action prompt is sent as the `systemInstruction`; the transcript is sent as the user message wrapped in `<transcript>…</transcript>` with a fixed preamble that it is data, never instructions. `ask_ai` is the exception: it executes the spoken content as an instruction. A trailing `:`/`：` in custom prompts is ignored.

```bash
# Inspect all effective Fcitx5 addon and daemon settings:
gemini-voice config show

# Choose whether dictation opens the action menu or commits raw text directly:
gemini-voice config set ime.mode menu    # Show Refine / Translate / Ask AI menu (default)
gemini-voice config set ime.mode direct  # Commit raw transcript immediately

# Configure Fcitx5 trigger shortcut, language, live transcription mode/vocabulary, VAD, prefetch, or proxy:
gemini-voice config set ime.trigger_key Alt+space
gemini-voice config set live.language auto          # auto (85+ languages), or comma-separated hints like cmn-Hans-CN,en-US (zh-CN aliases to cmn-Hans-CN)
gemini-voice config set live.mode VERBATIM          # VERBATIM (literal) or SMART (disfluency cleanup)
gemini-voice config set live.custom_vocabulary Fcitx5,PipeWire,Gemini
gemini-voice config set live.vad_silence_ms 1200       # Server-side VAD trailing-silence timeout in ms before auto-stop (0 disables auto-stop)
gemini-voice config set post_process.prefetch refine_fix_typos,refine_formal
gemini-voice config set connection.proxy socks5://127.0.0.1:1080

# Inspect current model routing and optionally query available models from Gemini API:
gemini-voice models
gemini-voice models --remote

# Choose models for live ASR, default post-processing, and individual actions:
gemini-voice models --live gemini-3.5-transcribe-live --default gemini-3.5-flash-lite --refine-formal gemini-3.1-pro-preview --as-prompt gemini-3.1-pro-preview
gemini-voice models --translate ja=gemini-3.5-flash/low --translate en=default  # Override or clear dynamic translate_<lang> targets (repeatable)
gemini-voice config set model.refine_fix_typos gemini-3.5-flash-lite
gemini-voice config set model.refine_formal gemini-3.5-flash/low   # Thinking effort via model suffix
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

# Explicitly select host key encryption (default is auto):
gemini-voice auth set --with-key host

# Verify decryption (prints masked fingerprint only, never the raw key):
gemini-voice auth verify

# Inspect full system diagnostics (config, credential, PipeWire, socket, systemd):
gemini-voice status

# Remove stored encrypted credential:
gemini-voice auth remove
```

For local development without `systemd-creds`:

```bash
GEMINI_API_KEY="your-key" ~/.local/bin/gemini-voice-daemon
```

## Usage & Hotkeys

1. Enable the **Gemini Voice** module in Fcitx5 (no need to switch away from your current input method such as Rime or `keyboard-us`).
2. Press **`Alt+space`** (configurable via `gemini-voice config set ime.trigger_key ...` or `fcitx5-configtool`) in any input method to start recording. Live transcription streams inside Fcitx5's floating candidate panel (`AuxUp` / `Preedit`) without modifying the application's input box until committed.
3. Press **`Alt+space`** or **`Return`** (or pause for `vad_silence_ms`) to stop recording:
   - In **`menu`** mode (`ShowActionMenu = True`), the Rime-style action menu opens:
     - `1` / `Space` / `Return`: Commit raw transcript (`[Raw]`)
     - `2` / `Right`: Open **Refine** submenu (`1` Fix typos, `2` Casual tone, `3` Formal tone, `4` Convert to structured AI prompt `as_prompt`; `Left` / `Escape` / `BackSpace` returns to Level 1)
     - `3`: Translate to English (`translate_en`)
     - `4`: Use spoken text directly as an AI prompt to generate a response (`ask_ai`)
     - `Escape`: Cancel and clear
   - In **`direct`** mode (`ShowActionMenu = False`), the raw transcript is committed immediately.

While recording is active, the IME captures **all** keystrokes; they are not forwarded to the focused application. This is deliberate: it keeps dictation clean and prevents stray typing from leaking into the app receiving the transcript. Press the trigger key (`Alt+space`) or `Return` (or pause for `vad_silence_ms`) to end recording and resume normal typing; `Escape` cancels it.

> **Note on hotkey conflicts:** Gemini Voice listens for key events in Fcitx5's `PreInputMethod` phase, which runs after Fcitx5's global hotkey watcher. If `Alt+space` (or your configured `ime.trigger_key`) collides with a Fcitx5 global hotkey, the global hotkey takes precedence and Gemini Voice will silently fail to trigger. Change `ime.trigger_key` or the conflicting Fcitx5 global shortcut if you encounter this.

## Daemon Configuration (`~/.config/gemini-voice/config.toml`)

Prompts, live transcription model (defaults to `gemini-3.5-transcribe-live`), `mode` (`VERBATIM` or `SMART`), optional `custom_vocabulary`, default `post_process.model`, optional `[post_process.models]` per-action overrides, proxy (`http://`, `https://`, `socks5://`), language (defaults to `auto`, or comma-separated BCP-47 hints such as `cmn-Hans-CN,en-US`; `zh-CN` aliases to `cmn-Hans-CN`), VAD silence timeout (`vad_silence_ms`), and `prefetch` actions live in `~/.config/gemini-voice/config.toml` and hot-reload automatically when saved.

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
- **Credential decryption failure**: Run `gemini-voice auth verify`; if `CREDENTIALS_DIRECTORY` is set by systemd, the encrypted credential file must exist and match the active host/user scope.
- **No audio captured**: Verify `pw-record --raw --rate=16000 --channels=1 --format=s16 - > /dev/null` works with your PipeWire microphone source.
- **Recording stops prematurely mid-sentence (or never auto-stops)**: Auto-stop is driven by the Live API's server-side voice activity detection, using `live.vad_silence_ms` as the trailing-silence duration. Check `journalctl --user -u gemini-voice.service -n 20` for `live session server vad auto-stop`. Increase `live.vad_silence_ms` (e.g. `gemini-voice config set live.vad_silence_ms 2000`) if it cuts you off during pauses, or set it to `0` to disable automatic detection and stop manually. The old local `live.vad_threshold` setting has been removed; existing config files that still contain it are read without error and the value is ignored.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
