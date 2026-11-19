#include "engine.h"
#include "json_util.h"

#include <cstdlib>
#include <functional>
#include <unistd.h>
#include <utility>
#include <vector>

#include <fcitx-config/iniparser.h>
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
}

GeminiVoiceEngine::~GeminiVoiceEngine() {
    alive_->store(false);
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

std::string
GeminiVoiceEngine::subModeIconImpl(const fcitx::InputMethodEntry & /*entry*/,
                                   fcitx::InputContext & /*ic*/) {
    return "audio-input-microphone";
}

std::string
GeminiVoiceEngine::subModeLabelImpl(const fcitx::InputMethodEntry & /*entry*/,
                                    fcitx::InputContext & /*ic*/) {
    switch (state_) {
    case State::RECORDING:
        return "REC";
    case State::STOPPING:
    case State::PROCESSING:
        return "AI";
    default:
        return "🎙";
    }
}

void GeminiVoiceEngine::activate(const fcitx::InputMethodEntry & /*entry*/,
                                 fcitx::InputContextEvent & /*event*/) {}

void GeminiVoiceEngine::deactivate(const fcitx::InputMethodEntry &entry,
                                   fcitx::InputContextEvent &event) {
    reset(entry, event);
}

void GeminiVoiceEngine::reset(const fcitx::InputMethodEntry & /*entry*/,
                              fcitx::InputContextEvent &event) {
    if (state_ == State::IDLE) {
        return;
    }
    auto *active = activeIC_.get();
    if (!active || event.inputContext() == active) {
        cancelActiveSession(active ? active : event.inputContext());
    }
}

void GeminiVoiceEngine::keyEvent(const fcitx::InputMethodEntry & /*entry*/,
                                 fcitx::KeyEvent &keyEvent) {
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

    std::string cmd =
        buildCommandJson("start_recording", {{"language", *config_.language}});
    if (!socketClient_->sendLine(cmd)) {
        FCITX_WARN() << "GeminiVoice failed to connect/send start_recording: "
                     << socketClient_->lastError();
        instance_->showCustomInputMethodInformation(
            ic, "Gemini Voice Daemon 未连接");
        return;
    }

    activeIC_ = ic->watch();
    partialText_.clear();
    rawText_.clear();
    state_ = State::RECORDING;
    setPreedit(ic, "[录音中...]", fcitx::TextFormatFlag::Underline);
}

void GeminiVoiceEngine::handleRecordingKey(fcitx::InputContext *ic,
                                           fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    const auto &key = keyEvent.key();

    if (key.checkKeyList(*config_.triggerKey) || key.check(FcitxKey_Return) ||
        key.check(FcitxKey_KP_Enter)) {
        if (!socketClient_->sendLine(buildCommandJson("stop_recording"))) {
            instance_->showCustomInputMethodInformation(
                ic, "Gemini Voice Daemon 连接断开");
            clearSession(ic);
            return;
        }
        state_ = State::STOPPING;
        setPreedit(ic,
                   partialText_.empty() ? "[整理转录中...]"
                                        : (partialText_ + " ..."),
                   fcitx::TextFormatFlag::Underline);
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

void GeminiVoiceEngine::setPreedit(fcitx::InputContext *ic,
                                   const std::string &text,
                                   fcitx::TextFormatFlags flags) {
    if (!ic) {
        return;
    }
    fcitx::Text formatted;
    if (!text.empty()) {
        formatted.append(text, flags);
        formatted.setCursor(static_cast<int>(text.size()));
    }

    auto &panel = ic->inputPanel();
    if (ic->capabilityFlags().test(fcitx::CapabilityFlag::Preedit)) {
        panel.setClientPreedit(formatted);
        panel.setPreedit(fcitx::Text());
        ic->updatePreedit();
    } else {
        panel.setClientPreedit(fcitx::Text());
        panel.setPreedit(formatted);
    }
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
    list->append<ActionCandidateWord>(fcitx::Text("[原文] " + preview),
                                      [this](fcitx::InputContext *targetIc) {
                                          std::string toCommit = rawText_;
                                          clearSession(targetIc);
                                          if (targetIc && !toCommit.empty()) {
                                              targetIc->commitString(toCommit);
                                          }
                                      });
    list->append<ActionCandidateWord>(
        fcitx::Text("[Refine ▸] 润色 (改错字 / 轻松 / 正式)"),
        [this](fcitx::InputContext *targetIc) { showRefineSubmenu(targetIc); });
    list->append<ActionCandidateWord>(fcitx::Text("[Translate] 译为英文"),
                                      [this](fcitx::InputContext *targetIc) {
                                          triggerPostProcess(targetIc,
                                                             "translate_en");
                                      });
    list->append<ActionCandidateWord>(
        fcitx::Text("[As Prompt] 转为结构化提示词"),
        [this](fcitx::InputContext *targetIc) {
            triggerPostProcess(targetIc, "as_prompt");
        });

    list->setGlobalCursorIndex(0);

    auto &panel = ic->inputPanel();
    panel.setAuxUp(fcitx::Text());
    panel.setAuxDown(fcitx::Text());
    panel.setCandidateList(std::move(list));
    setPreedit(ic, rawText_, fcitx::TextFormatFlag::Underline);
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
        fcitx::Text("[仅改错字] 修正同音错别字与标点，保留口语原貌"),
        [this](fcitx::InputContext *targetIc) {
            triggerPostProcess(targetIc, "refine_fix_typos");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text("[轻松语气] 自然流畅，适合即时通讯聊天"),
        [this](fcitx::InputContext *targetIc) {
            triggerPostProcess(targetIc, "refine_casual");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text("[正式语气] 严谨专业，适合邮件与工作文档"),
        [this](fcitx::InputContext *targetIc) {
            triggerPostProcess(targetIc, "refine_formal");
        });

    list->setGlobalCursorIndex(0);

    auto &panel = ic->inputPanel();
    panel.setAuxUp(fcitx::Text("Refine ▸"));
    panel.setAuxDown(fcitx::Text());
    panel.setCandidateList(std::move(list));
    setPreedit(ic, rawText_, fcitx::TextFormatFlag::Underline);
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
            ic, "Gemini Voice Daemon 未连接");
        showActionMenu(ic);
        return;
    }

    state_ = State::PROCESSING;
    ic->inputPanel().setAuxUp(fcitx::Text());
    ic->inputPanel().setAuxDown(fcitx::Text());
    ic->inputPanel().setCandidateList(nullptr);
    setPreedit(ic, "[AI 处理中...]", fcitx::TextFormatFlag::Underline);
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
        setPreedit(ic, partialText_.empty() ? "[录音中...]" : partialText_,
                   fcitx::TextFormatFlag::Underline);
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
            instance_->showCustomInputMethodInformation(ic, "未检测到语音内容");
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
        std::string errMsg = (it != msg.end()) ? it->second : "未知错误";
        if (state_ == State::PROCESSING && ic && !rawText_.empty()) {
            instance_->showCustomInputMethodInformation(ic, "AI 处理失败: " +
                                                                errMsg);
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
    }
}

void GeminiVoiceEngine::onDaemonDisconnect() {
    if (state_ == State::IDLE) {
        return;
    }
    auto *ic = activeIC_.get();
    if (state_ == State::PROCESSING && ic && !rawText_.empty()) {
        instance_->showCustomInputMethodInformation(
            ic, "与 Gemini Voice Daemon 断开连接");
        showActionMenu(ic);
        return;
    }
    if (ic) {
        instance_->showCustomInputMethodInformation(
            ic, "与 Gemini Voice Daemon 断开连接");
    }
    clearSession(ic);
}

} // namespace fcitx_gemini_voice
