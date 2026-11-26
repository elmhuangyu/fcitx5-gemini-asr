#include "engine.h"
#include "engine_panel.h"
#include "json_util.h"

#include <fcitx-utils/i18n.h>
#include <fcitx-utils/log.h>
#include <fcitx/inputpanel.h>
#include <fcitx/text.h>

namespace fcitx_gemini_voice {

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
        translateTargets_ = parseTranslateTargets(line);
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
