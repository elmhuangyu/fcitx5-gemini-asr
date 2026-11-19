#ifndef FCITX5_GEMINI_VOICE_ENGINE_H
#define FCITX5_GEMINI_VOICE_ENGINE_H

#include "socket_client.h"

#include <atomic>
#include <memory>
#include <string>

#include <fcitx-config/configuration.h>
#include <fcitx-config/option.h>
#include <fcitx-utils/key.h>
#include <fcitx-utils/trackableobject.h>
#include <fcitx/addoninstance.h>
#include <fcitx/inputcontext.h>
#include <fcitx/inputmethodengine.h>
#include <fcitx/instance.h>

namespace fcitx_gemini_voice {

FCITX_CONFIGURATION(
    GeminiVoiceConfig,
    fcitx::KeyListOption triggerKey{
        this,
        "TriggerKey",
        "Trigger Key",
        {fcitx::Key("Alt+space")},
        fcitx::KeyListConstrain({fcitx::KeyConstrainFlag::AllowModifierLess,
                                 fcitx::KeyConstrainFlag::AllowModifierOnly})};
    fcitx::Option<bool> showActionMenu{this, "ShowActionMenu",
                                       "Show LLM Action Menu", true};
    fcitx::Option<std::string> socketPath{this, "SocketPath",
                                          "Daemon Unix Socket Path",
                                          "$XDG_RUNTIME_DIR/gemini-ime.sock"};
    fcitx::Option<std::string> language{this, "Language",
                                        "Transcription Language", ""};);

class GeminiVoiceEngine : public fcitx::InputMethodEngineV2 {
public:
    enum class State {
        IDLE,
        RECORDING,
        STOPPING,
        ACTION_MENU,
        SUBMENU_REFINE,
        PROCESSING,
    };

    explicit GeminiVoiceEngine(fcitx::Instance *instance);
    ~GeminiVoiceEngine() override;

    void keyEvent(const fcitx::InputMethodEntry &entry,
                  fcitx::KeyEvent &keyEvent) override;
    void activate(const fcitx::InputMethodEntry &entry,
                  fcitx::InputContextEvent &event) override;
    void deactivate(const fcitx::InputMethodEntry &entry,
                    fcitx::InputContextEvent &event) override;
    void reset(const fcitx::InputMethodEntry &entry,
               fcitx::InputContextEvent &event) override;

    std::string subModeIconImpl(const fcitx::InputMethodEntry &entry,
                                fcitx::InputContext &ic) override;
    std::string subModeLabelImpl(const fcitx::InputMethodEntry &entry,
                                 fcitx::InputContext &ic) override;

    const fcitx::Configuration *getConfig() const override { return &config_; }
    void setConfig(const fcitx::RawConfig &config) override;
    void reloadConfig() override;

    State state() const { return state_; }
    const GeminiVoiceConfig &config() const { return config_; }

private:
    std::string resolveSocketPath() const;
    void onDaemonMessage(const std::string &line);
    void onDaemonDisconnect();

    void handleIdleKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleRecordingKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleStoppingKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleActionMenuKey(fcitx::InputContext *ic,
                             fcitx::KeyEvent &keyEvent);
    void handleSubmenuRefineKey(fcitx::InputContext *ic,
                                fcitx::KeyEvent &keyEvent);
    void handleProcessingKey(fcitx::InputContext *ic,
                             fcitx::KeyEvent &keyEvent);

    void setPreedit(fcitx::InputContext *ic, const std::string &text,
                    fcitx::TextFormatFlags flags);
    void clearSession(fcitx::InputContext *ic);
    void showActionMenu(fcitx::InputContext *ic);
    void showRefineSubmenu(fcitx::InputContext *ic);
    void triggerPostProcess(fcitx::InputContext *ic, const std::string &action);
    void cancelActiveSession(fcitx::InputContext *ic);

    fcitx::Instance *instance_;
    GeminiVoiceConfig config_;
    std::shared_ptr<std::atomic<bool>> alive_;
    std::unique_ptr<SocketClient> socketClient_;

    State state_ = State::IDLE;
    fcitx::TrackableObjectReference<fcitx::InputContext> activeIC_;
    std::string partialText_;
    std::string rawText_;
};

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_ENGINE_H
