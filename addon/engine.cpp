#include "engine.h"
#include "json_util.h"

#include <cstdlib>
#include <functional>
#include <unistd.h>
#include <utility>
#include <vector>

#include <fcitx-config/iniparser.h>
#include <fcitx-utils/i18n.h>
#include <fcitx-utils/keysym.h>
#include <fcitx-utils/log.h>
#include <fcitx-utils/utf8.h>
#include <fcitx/candidatelist.h>
#include <fcitx/inputpanel.h>
#include <fcitx/text.h>

namespace fcitx_gemini_voice {

namespace {

constexpr const char *kConfigFile = "conf/gemini-voice.conf";

class ActionCandidateWord : public fcitx::CandidateWord {
public:
    using Callback = std::function<void(fcitx::InputContext *)>;

    ActionCandidateWord(fcitx::Text text, Callback cb)
        : fcitx::CandidateWord(std::move(text)), cb_(std::move(cb)) {}

    void select(fcitx::InputContext *ic) const override {
        if (cb_) {
            cb_(ic);
        }
    }

private:
    Callback cb_;
};

fcitx::KeyList defaultSelectionKeys() {
    return {
        fcitx::Key(FcitxKey_1), fcitx::Key(FcitxKey_2), fcitx::Key(FcitxKey_3),
        fcitx::Key(FcitxKey_4), fcitx::Key(FcitxKey_5), fcitx::Key(FcitxKey_6),
        fcitx::Key(FcitxKey_7), fcitx::Key(FcitxKey_8), fcitx::Key(FcitxKey_9),
    };
}

std::string truncateUtf8(const std::string &str, size_t maxChars) {
    if (!fcitx::utf8::validate(str)) {
        return str.size() > maxChars ? str.substr(0, maxChars) + "..." : str;
    }
    if (fcitx::utf8::length(str) <= maxChars) {
        return str;
    }
    ssize_t bytes = fcitx::utf8::ncharByteLength(str.begin(), maxChars);
    if (bytes < 0) {
        return str;
    }
    return str.substr(0, static_cast<size_t>(bytes)) + "...";
}

int charDisplayWidth(uint32_t cp) {
    if (cp == 0) {
        return 0;
    }
    if ((cp >= 0x1100 && cp <= 0x115F) || cp == 0x2329 || cp == 0x232A ||
        (cp >= 0x2E80 && cp <= 0x303E) || (cp >= 0x3040 && cp <= 0x3247) ||
        (cp >= 0x3250 && cp <= 0x4DBF) || (cp >= 0x4E00 && cp <= 0xA4CF) ||
        (cp >= 0xA960 && cp <= 0xA97F) || (cp >= 0xAC00 && cp <= 0xD7A3) ||
        (cp >= 0xF900 && cp <= 0xFAFF) || (cp >= 0xFE10 && cp <= 0xFE19) ||
        (cp >= 0xFE30 && cp <= 0xFE6F) || (cp >= 0xFF01 && cp <= 0xFF60) ||
        (cp >= 0xFFE0 && cp <= 0xFFE6) || (cp >= 0x1F300 && cp <= 0x1FAFF) ||
        (cp >= 0x20000 && cp <= 0x3FFFD)) {
        return 2;
    }
    return 1;
}

std::string wrapPreeditText(const std::string &text, int maxWidth) {
    if (maxWidth <= 0 || text.empty() || !fcitx::utf8::validate(text)) {
        return text;
    }

    std::string out;
    out.reserve(text.size() + 8);
    int lineWidth = 0;
    size_t lineStartOutIdx = 0;
    size_t lastSpaceOutIdx = 0;
    int widthSinceSpace = 0;

    auto it = text.begin();
    const auto end = text.end();
    while (it != end) {
        uint32_t cp = 0;
        auto next = fcitx::utf8::getNextChar(it, end, &cp);
        if (!fcitx::utf8::isValidChar(cp)) {
            out.append(it, next);
            it = next;
            continue;
        }
        if (cp == '\n') {
            out.push_back('\n');
            lineWidth = 0;
            lineStartOutIdx = out.size();
            lastSpaceOutIdx = 0;
            widthSinceSpace = 0;
            it = next;
            continue;
        }

        const int w = charDisplayWidth(cp);
        if (lineWidth > 0 && lineWidth + w > maxWidth) {
            if (cp == ' ') {
                out.push_back('\n');
                lineWidth = 0;
                lineStartOutIdx = out.size();
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
                it = next;
                continue;
            }
            if (lastSpaceOutIdx > lineStartOutIdx &&
                widthSinceSpace <= maxWidth / 3 && cp < 0x80) {
                out[lastSpaceOutIdx - 1] = '\n';
                lineStartOutIdx = lastSpaceOutIdx;
                lineWidth = widthSinceSpace;
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
            } else {
                out.push_back('\n');
                lineWidth = 0;
                lineStartOutIdx = out.size();
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
            }
        }

        out.append(it, next);
        lineWidth += w;
        if (cp == ' ') {
            lastSpaceOutIdx = out.size();
            widthSinceSpace = 0;
        } else if (lastSpaceOutIdx > lineStartOutIdx) {
            widthSinceSpace += w;
        }
        it = next;
    }
    return out;
}

} // namespace

GeminiVoiceEngine::GeminiVoiceEngine(fcitx::Instance *instance)
    : instance_(instance), alive_(std::make_shared<std::atomic<bool>>(true)) {
    reloadConfig();

    auto alive = alive_;
    socketClient_ = std::make_unique<SocketClient>(
        resolveSocketPath(),
        [this, alive](const std::string &line) {
            if (!alive->load()) {
                return;
            }
            instance_->eventDispatcher().schedule([this, alive, line]() {
                if (!alive->load()) {
                    return;
                }
                onDaemonMessage(line);
            });
        },
        [this, alive]() {
            if (!alive->load()) {
                return;
            }
            instance_->eventDispatcher().schedule([this, alive]() {
                if (!alive->load()) {
                    return;
                }
                onDaemonDisconnect();
            });
        });

    eventHandlers_.push_back(instance_->watchEvent(
        fcitx::EventType::InputContextKeyEvent,
        fcitx::EventWatcherPhase::PreInputMethod, [this](fcitx::Event &event) {
            onKeyEvent(static_cast<fcitx::KeyEvent &>(event));
        }));

    auto cancelOnContextEvent = [this](fcitx::Event &event) {
        if (state_ == State::IDLE) {
            return;
        }
        auto &icEvent = static_cast<fcitx::InputContextEvent &>(event);
        auto *active = activeIC_.get();
        if (!active || icEvent.inputContext() == active) {
            cancelActiveSession(active ? active : icEvent.inputContext());
        }
    };
    eventHandlers_.push_back(instance_->watchEvent(
        fcitx::EventType::InputContextFocusOut,
        fcitx::EventWatcherPhase::Default, cancelOnContextEvent));
    eventHandlers_.push_back(instance_->watchEvent(
        fcitx::EventType::InputContextReset, fcitx::EventWatcherPhase::Default,
        cancelOnContextEvent));
    eventHandlers_.push_back(instance_->watchEvent(
        fcitx::EventType::InputContextSwitchInputMethod,
        fcitx::EventWatcherPhase::Default, cancelOnContextEvent));
}

GeminiVoiceEngine::~GeminiVoiceEngine() {
    alive_->store(false);
    eventHandlers_.clear();
    if (auto *ic = activeIC_.get()) {
        ic->inputPanel().reset();
    }
    activeIC_.unwatch();
    if (socketClient_) {
        socketClient_->stop();
    }
}

std::string GeminiVoiceEngine::resolveSocketPath() const {
    std::string raw = *config_.socketPath;
    if (raw.empty()) {
        raw = "$XDG_RUNTIME_DIR/gemini-ime.sock";
    }

    const char *xdgRuntime = std::getenv("XDG_RUNTIME_DIR");
    std::string runtimeDir =
        (xdgRuntime && xdgRuntime[0] != '\0')
            ? std::string(xdgRuntime)
            : ("/run/user/" +
               std::to_string(static_cast<unsigned long>(::getuid())));

    const std::string token = "$XDG_RUNTIME_DIR";
    size_t pos = 0;
    while ((pos = raw.find(token, pos)) != std::string::npos) {
        raw.replace(pos, token.size(), runtimeDir);
        pos += runtimeDir.size();
    }

    if (raw.size() >= 2 && raw[0] == '~' && raw[1] == '/') {
        const char *home = std::getenv("HOME");
        if (home && home[0] != '\0') {
            raw.replace(0, 1, home);
        }
    }
    return raw;
}

void GeminiVoiceEngine::reloadConfig() {
    fcitx::readAsIni(config_, kConfigFile);
    if (socketClient_) {
        socketClient_->setSocketPath(resolveSocketPath());
    }
}

void GeminiVoiceEngine::setConfig(const fcitx::RawConfig &config) {
    config_.load(config, true);
    fcitx::safeSaveAsIni(config_, kConfigFile);
    if (socketClient_) {
        socketClient_->setSocketPath(resolveSocketPath());
    }
}

void GeminiVoiceEngine::onKeyEvent(fcitx::KeyEvent &keyEvent) {
    if (keyEvent.isRelease()) {
        return;
    }

    auto *ic = keyEvent.inputContext();
    if (!ic) {
        return;
    }

    if (state_ != State::IDLE && activeIC_.get() != ic) {
        cancelActiveSession(activeIC_.get());
    }

    switch (state_) {
    case State::IDLE:
        handleIdleKey(ic, keyEvent);
        break;
    case State::RECORDING:
        handleRecordingKey(ic, keyEvent);
        break;
    case State::STOPPING:
        handleStoppingKey(ic, keyEvent);
        break;
    case State::ACTION_MENU:
        handleActionMenuKey(ic, keyEvent);
        break;
    case State::SUBMENU_REFINE:
        handleSubmenuRefineKey(ic, keyEvent);
        break;
    case State::PROCESSING:
        handleProcessingKey(ic, keyEvent);
        break;
    }
}

void GeminiVoiceEngine::handleIdleKey(fcitx::InputContext *ic,
                                      fcitx::KeyEvent &keyEvent) {
    if (!keyEvent.key().checkKeyList(*config_.triggerKey)) {
        return;
    }
    keyEvent.filterAndAccept();
    // Clear any active preedit from the underlying input method before
    // starting a voice session; state_ is still IDLE so InputContextReset
    // will not self-cancel.
    ic->reset();

    std::string cmd =
        buildCommandJson("start_recording", {{"language", *config_.language}});
    if (!socketClient_->sendLine(cmd)) {
        FCITX_WARN() << "GeminiVoice failed to connect/send start_recording: "
                     << socketClient_->lastError();
        instance_->showCustomInputMethodInformation(
            ic, _("Gemini Voice Daemon not connected"));
        return;
    }

    activeIC_ = ic->watch();
    partialText_.clear();
    rawText_.clear();
    state_ = State::RECORDING;
    setPanelDisplay(ic, *config_.recordingPrefix, "");
}

void GeminiVoiceEngine::handleRecordingKey(fcitx::InputContext *ic,
                                           fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    const auto &key = keyEvent.key();

    if (key.checkKeyList(*config_.triggerKey) || key.check(FcitxKey_Return) ||
        key.check(FcitxKey_KP_Enter)) {
        if (!socketClient_->sendLine(buildCommandJson("stop_recording"))) {
            instance_->showCustomInputMethodInformation(
                ic, _("Gemini Voice Daemon disconnected"));
            clearSession(ic);
            return;
        }
        state_ = State::STOPPING;
        setPanelDisplay(ic, _("[Finalizing transcript...]"), partialText_);
        return;
    }

    if (key.check(FcitxKey_Escape)) {
        cancelActiveSession(ic);
    }
}

void GeminiVoiceEngine::handleStoppingKey(fcitx::InputContext *ic,
                                          fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    if (keyEvent.key().check(FcitxKey_Escape)) {
        cancelActiveSession(ic);
    }
}

void GeminiVoiceEngine::handleActionMenuKey(fcitx::InputContext *ic,
                                            fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    auto candList = ic->inputPanel().candidateList();
    if (!candList) {
        clearSession(ic);
        return;
    }

    const auto &key = keyEvent.key();
    int digit = key.digitSelection();
    if (digit >= 0 && digit < candList->size()) {
        candList->candidate(digit).select(ic);
        return;
    }

    if (key.check(FcitxKey_space) || key.check(FcitxKey_Return) ||
        key.check(FcitxKey_KP_Enter)) {
        int idx = candList->cursorIndex();
        if (idx >= 0 && idx < candList->size()) {
            candList->candidate(idx).select(ic);
        }
        return;
    }

    if (key.check(FcitxKey_Right)) {
        showRefineSubmenu(ic);
        return;
    }

    if (key.check(FcitxKey_Up)) {
        if (auto *movable = candList->toCursorMovable()) {
            movable->prevCandidate();
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return;
    }

    if (key.check(FcitxKey_Down)) {
        if (auto *movable = candList->toCursorMovable()) {
            movable->nextCandidate();
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return;
    }

    if (key.check(FcitxKey_Escape)) {
        cancelActiveSession(ic);
    }
}

void GeminiVoiceEngine::handleSubmenuRefineKey(fcitx::InputContext *ic,
                                               fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    auto candList = ic->inputPanel().candidateList();
    if (!candList) {
        clearSession(ic);
        return;
    }

    const auto &key = keyEvent.key();
    int digit = key.digitSelection();
    if (digit >= 0 && digit < candList->size()) {
        candList->candidate(digit).select(ic);
        return;
    }

    if (key.check(FcitxKey_space) || key.check(FcitxKey_Return) ||
        key.check(FcitxKey_KP_Enter)) {
        int idx = candList->cursorIndex();
        if (idx >= 0 && idx < candList->size()) {
            candList->candidate(idx).select(ic);
        }
        return;
    }

    if (key.check(FcitxKey_Up)) {
        if (auto *movable = candList->toCursorMovable()) {
            movable->prevCandidate();
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return;
    }

    if (key.check(FcitxKey_Down)) {
        if (auto *movable = candList->toCursorMovable()) {
            movable->nextCandidate();
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return;
    }

    if (key.check(FcitxKey_Left) || key.check(FcitxKey_Escape) ||
        key.check(FcitxKey_BackSpace)) {
        showActionMenu(ic);
    }
}

void GeminiVoiceEngine::handleProcessingKey(fcitx::InputContext *ic,
                                            fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    if (keyEvent.key().check(FcitxKey_Escape)) {
        cancelActiveSession(ic);
    }
}

void GeminiVoiceEngine::setPanelDisplay(fcitx::InputContext *ic,
                                        const std::string &auxUp,
                                        const std::string &preeditText) {
    if (!ic) {
        return;
    }
    const std::string wrappedPreedit =
        wrapPreeditText(preeditText, *config_.preeditMaxWidth);

    std::string firstLine;
    std::string restLines;
    if (!wrappedPreedit.empty()) {
        const size_t nl = wrappedPreedit.find('\n');
        if (nl == std::string::npos) {
            firstLine = wrappedPreedit;
        } else {
            firstLine = wrappedPreedit.substr(0, nl);
            restLines = wrappedPreedit.substr(nl + 1);
        }
    }

    // Fcitx5 classicui forces upperLayout_ (auxUp + preedit) into Pango
    // single-paragraph mode, which renders '\n' as a visible return glyph
    // instead of breaking lines. Keep only the first line in preedit and place
    // wrapped continuation lines in auxDown (lowerLayout_, which is multiline).
    fcitx::Text formattedPreedit;
    if (!firstLine.empty()) {
        formattedPreedit.append(firstLine, fcitx::TextFormatFlag::Underline);
        if (restLines.empty()) {
            formattedPreedit.setCursor(static_cast<int>(firstLine.size()));
        }
    }

    fcitx::Text formattedAuxDown;
    if (!restLines.empty()) {
        formattedAuxDown.append(restLines, fcitx::TextFormatFlag::Underline);
    }

    std::string displayAuxUp = auxUp;
    if (!displayAuxUp.empty() && !firstLine.empty() &&
        displayAuxUp.back() != ' ' && displayAuxUp.back() != '\t') {
        displayAuxUp.push_back(' ');
    }

    auto &panel = ic->inputPanel();
    panel.setClientPreedit(fcitx::Text());
    panel.setAuxUp(displayAuxUp.empty() ? fcitx::Text()
                                        : fcitx::Text(displayAuxUp));
    panel.setPreedit(formattedPreedit);
    panel.setAuxDown(formattedAuxDown);
    ic->updatePreedit();
    ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
}

void GeminiVoiceEngine::clearSession(fcitx::InputContext *ic) {
    state_ = State::IDLE;
    partialText_.clear();
    rawText_.clear();
    activeIC_.unwatch();

    if (ic) {
        ic->inputPanel().reset();
        ic->updatePreedit();
        ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
    }
}

void GeminiVoiceEngine::cancelActiveSession(fcitx::InputContext *ic) {
    if (socketClient_) {
        socketClient_->sendLine(buildCommandJson("cancel"));
    }
    clearSession(ic);
}

void GeminiVoiceEngine::showActionMenu(fcitx::InputContext *ic) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    state_ = State::ACTION_MENU;
    auto list = std::make_unique<fcitx::CommonCandidateList>();
    list->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
    list->setPageSize(9);
    list->setSelectionKey(defaultSelectionKeys());

    const std::string preview = truncateUtf8(rawText_, 40);
    list->append<ActionCandidateWord>(
        fcitx::Text(std::string(_("[Raw]")) + " " + preview),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            std::string toCommit = rawText_;
            clearSession(targetIc);
            if (targetIc && !toCommit.empty()) {
                targetIc->commitString(toCommit);
            }
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[Refine ▸] Polish (Fix typos / Casual / Formal)")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            showRefineSubmenu(targetIc);
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[Translate] Translate to English")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "translate_en");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[As Prompt] Convert to structured prompt")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "as_prompt");
        });

    list->setGlobalCursorIndex(0);

    auto &panel = ic->inputPanel();
    panel.setAuxDown(fcitx::Text());
    panel.setCandidateList(std::move(list));
    setPanelDisplay(ic, "", rawText_);
}

void GeminiVoiceEngine::showRefineSubmenu(fcitx::InputContext *ic) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    state_ = State::SUBMENU_REFINE;
    auto list = std::make_unique<fcitx::CommonCandidateList>();
    list->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
    list->setPageSize(9);
    list->setSelectionKey(defaultSelectionKeys());

    list->append<ActionCandidateWord>(
        fcitx::Text(_(
            "[Fix Typos] Fix typos and punctuation only, keeping spoken tone")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_fix_typos");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[Casual] Natural and relaxed tone for chat")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_casual");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(
            _("[Formal] Clear and professional tone for email and docs")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_formal");
        });

    list->setGlobalCursorIndex(0);

    auto &panel = ic->inputPanel();
    panel.setAuxDown(fcitx::Text());
    panel.setCandidateList(std::move(list));
    setPanelDisplay(ic, _("Refine ▸"), rawText_);
}

void GeminiVoiceEngine::triggerPostProcess(fcitx::InputContext *ic,
                                           const std::string &action) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    std::string cmd = buildCommandJson("post_process", {{"action", action}});
    if (!socketClient_->sendLine(cmd)) {
        instance_->showCustomInputMethodInformation(
            ic, _("Gemini Voice Daemon not connected"));
        showActionMenu(ic);
        return;
    }

    state_ = State::PROCESSING;
    ic->inputPanel().setAuxDown(fcitx::Text());
    ic->inputPanel().setCandidateList(nullptr);
    setPanelDisplay(ic, _("[AI Processing...]"), "");
}

void GeminiVoiceEngine::onDaemonMessage(const std::string &line) {
    auto parsed = parseFlatJsonObject(line);
    if (!parsed.has_value()) {
        FCITX_WARN() << "GeminiVoice ignored malformed daemon line";
        return;
    }

    const auto &msg = *parsed;
    auto evIt = msg.find("event");
    if (evIt == msg.end()) {
        return;
    }
    const std::string &ev = evIt->second;

    auto *ic = activeIC_.get();

    if (ev == "partial_text") {
        if (state_ != State::RECORDING) {
            return;
        }
        if (!ic) {
            cancelActiveSession(nullptr);
            return;
        }
        auto it = msg.find("text");
        partialText_ = (it != msg.end()) ? it->second : "";
        setPanelDisplay(ic, *config_.recordingPrefix, partialText_);
        return;
    }

    if (ev == "transcribe_done") {
        if (state_ != State::RECORDING && state_ != State::STOPPING) {
            return;
        }
        if (!ic) {
            clearSession(nullptr);
            return;
        }
        auto it = msg.find("raw_text");
        std::string raw = (it != msg.end()) ? it->second : "";
        if (raw.empty()) {
            clearSession(ic);
            instance_->showCustomInputMethodInformation(
                ic, _("No speech detected"));
            return;
        }
        if (!*config_.showActionMenu) {
            clearSession(ic);
            ic->commitString(raw);
            return;
        }
        rawText_ = std::move(raw);
        showActionMenu(ic);
        return;
    }

    if (ev == "final_result") {
        if (state_ != State::PROCESSING) {
            return;
        }
        auto it = msg.find("text");
        std::string finalText = (it != msg.end()) ? it->second : "";
        clearSession(ic);
        if (ic && !finalText.empty()) {
            ic->commitString(finalText);
        }
        return;
    }

    if (ev == "error") {
        auto it = msg.find("message");
        std::string errMsg =
            (it != msg.end()) ? it->second : _("Unknown error");
        if (state_ == State::PROCESSING && ic && !rawText_.empty()) {
            instance_->showCustomInputMethodInformation(
                ic, std::string(_("AI processing failed: ")) + errMsg);
            showActionMenu(ic);
            return;
        }
        if (state_ != State::IDLE) {
            if (ic) {
                instance_->showCustomInputMethodInformation(
                    ic, "Gemini Voice: " + errMsg);
            }
            clearSession(ic);
        }
        return;
    }

    if (ev == "state_changed") {
        auto it = msg.find("state");
        if (it == msg.end() || it->second != "idle") {
            // "recording"/"processing" are echoes of commands this engine
            // itself sent; nothing to do.
            return;
        }
        if (state_ != State::RECORDING && state_ != State::STOPPING &&
            state_ != State::PROCESSING) {
            // Idle echoes a transition the engine already handled itself
            // (its own cancel, the error path, or a delivered result), so
            // acting here would double-reset the normal flow.
            return;
        }
        // Daemon reports idle while a session is still in flight and no
        // partial/final result arrived: unexpected daemon-side reset
        // (e.g. daemon restart or cancelled session). Recover to idle
        // with feedback.
        clearSession(ic);
        if (ic) {
            instance_->showCustomInputMethodInformation(
                ic, _("Voice session ended unexpectedly"));
        }
    }
}

void GeminiVoiceEngine::onDaemonDisconnect() {
    if (state_ == State::IDLE) {
        return;
    }
    auto *ic = activeIC_.get();
    if (state_ == State::PROCESSING && ic && !rawText_.empty()) {
        instance_->showCustomInputMethodInformation(
            ic, _("Disconnected from Gemini Voice Daemon"));
        showActionMenu(ic);
        return;
    }
    if (ic) {
        instance_->showCustomInputMethodInformation(
            ic, _("Disconnected from Gemini Voice Daemon"));
    }
    clearSession(ic);
}

} // namespace fcitx_gemini_voice
