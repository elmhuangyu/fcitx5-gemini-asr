#include "engine.h"
#include "json_util.h"

#include <cstdlib>
#include <unistd.h>

#include <fcitx-config/iniparser.h>
#include <fcitx-utils/i18n.h>
#include <fcitx/inputpanel.h>

namespace fcitx_gemini_voice {

namespace {

constexpr const char *kConfigFile = "conf/gemini-voice.conf";

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

void GeminiVoiceEngine::clearSession(fcitx::InputContext *ic) {
    state_ = State::IDLE;
    partialText_.clear();
    rawText_.clear();
    translateTargets_.clear();
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

} // namespace fcitx_gemini_voice
