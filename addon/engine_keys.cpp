#include "engine.h"
#include "engine_panel.h"
#include "json_util.h"

#include <fcitx-utils/i18n.h>
#include <fcitx-utils/keysym.h>
#include <fcitx-utils/log.h>
#include <fcitx/inputpanel.h>

namespace fcitx_gemini_voice {

void GeminiVoiceEngine::onKeyEvent(fcitx::KeyEvent &keyEvent) {
    auto *ic = keyEvent.inputContext();
    if (!ic) {
        return;
    }

    if (keyEvent.isRelease()) {
        if (state_ != State::IDLE && activeIC_.get() == ic) {
            keyEvent.filterAndAccept();
        }
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
    case State::SUBMENU_TRANSLATE:
        handleSubmenuKey(ic, keyEvent);
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
            ic, _("Gemini Voice Daemon not connected"));
        return;
    }

    // Clear any active preedit from the underlying input method only after
    // start_recording succeeds; state_ is still IDLE so InputContextReset
    // will not self-cancel.
    ic->reset();

    activeIC_ = ic->watch();
    partialText_.clear();
    rawText_.clear();
    translateTargets_.clear();
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
    if (handleCandidateMenuKey(ic, *candList, key)) {
        return;
    }

    if (key.check(FcitxKey_Right)) {
        const int translateIdx = (isHeaderAtPageStart(*candList) ? 1 : 0) + 2;
        if (candList->cursorIndex() == translateIdx &&
            effectiveTranslateTargets().size() > 1) {
            showTranslateSubmenu(ic);
        } else {
            showRefineSubmenu(ic);
        }
        return;
    }

    if (key.check(FcitxKey_Escape)) {
        cancelActiveSession(ic);
    }
}

void GeminiVoiceEngine::handleSubmenuKey(fcitx::InputContext *ic,
                                         fcitx::KeyEvent &keyEvent) {
    keyEvent.filterAndAccept();
    auto candList = ic->inputPanel().candidateList();
    if (!candList) {
        clearSession(ic);
        return;
    }

    const auto &key = keyEvent.key();
    if (handleCandidateMenuKey(ic, *candList, key)) {
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

} // namespace fcitx_gemini_voice
