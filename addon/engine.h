#ifndef FCITX5_GEMINI_VOICE_ENGINE_H
#define FCITX5_GEMINI_VOICE_ENGINE_H

#include "json_util.h"
#include "socket_client.h"

#include <atomic>
#include <memory>
#include <string>
#include <vector>

#include <fcitx-config/configuration.h>
#include <fcitx-config/enum.h>
#include <fcitx-config/option.h>
#ifndef FCITX_GETTEXT_DOMAIN
#define FCITX_GETTEXT_DOMAIN "fcitx5-gemini-voice"
#endif
#include <fcitx-utils/i18n.h>
#include <fcitx-utils/key.h>
#include <fcitx-utils/trackableobject.h>
#include <fcitx/addoninstance.h>
#include <fcitx/event.h>
#include <fcitx/inputcontext.h>
#include <fcitx/instance.h>

namespace fcitx {
class CandidateList;
class CommonCandidateList;
} // namespace fcitx

namespace fcitx_gemini_voice {

// The addon itself only consumes the top-level UI keys (TriggerKey,
// ShowActionMenu, SocketPath, Language, RecordingPrefix, PreeditMaxWidth,
// TranslateLanguages). Every other option below is consumed by the daemon,
// which watches this file; it is declared here so Fcitx5's config UI and
// safeSaveAsIni keep (and let users edit) the daemon's settings.
enum class LiveMode { VERBATIM, SMART };
FCITX_CONFIG_ENUM_NAME_WITH_I18N(LiveMode, N_("VERBATIM"), N_("SMART"));

FCITX_CONFIGURATION(
    LiveTranscriptionConfig,
    fcitx::Option<std::string> model{this, "Model", _("Live Model"),
                                     "gemini-3.5-transcribe-live"};
    fcitx::Option<LiveMode, fcitx::NoConstrain<LiveMode>,
                  fcitx::DefaultMarshaller<LiveMode>, LiveModeI18NAnnotation>
        mode{this,
             "Mode",
             _("Transcription Mode"),
             LiveMode::VERBATIM,
             {},
             {},
             {}};
    fcitx::Option<std::vector<std::string>> customVocabulary{
        this, "CustomVocabulary", _("Custom Vocabulary"), {}};
    fcitx::Option<int, fcitx::IntConstrain> vadSilenceMs{
        this, "VadSilenceMs", _("Auto-stop Silence (ms, 0 disables)"), 1200,
        fcitx::IntConstrain(0, 60000)};);

FCITX_CONFIGURATION(
    ActionOverrideConfig,
    fcitx::Option<std::string> name{this, "Name", _("Action Name")};
    fcitx::Option<std::string> prompt{this, "Prompt",
                                      _("Prompt (empty uses built-in)")};
    fcitx::Option<std::string> model{
        this, "Model", _("Model (empty uses default post-process model)")};);

FCITX_CONFIGURATION(
    PostProcessConfig,
    fcitx::Option<std::string> model{this, "Model", _("Default Model"),
                                     "gemini-3.5-flash-lite"};
    fcitx::Option<std::vector<std::string>> prefetch{
        this,
        "Prefetch",
        _("Prefetched Actions"),
        {"refine_fix_typos", "refine_formal"}};
    fcitx::Option<std::vector<ActionOverrideConfig>,
                  fcitx::NoConstrain<std::vector<ActionOverrideConfig>>,
                  fcitx::DefaultMarshaller<std::vector<ActionOverrideConfig>>,
                  fcitx::ListDisplayOptionAnnotation>
        actions{this,
                "Actions",
                _("Action Overrides"),
                {},
                {},
                {},
                fcitx::ListDisplayOptionAnnotation("Name")};);

FCITX_CONFIGURATION(
    ConnectionConfig,
    fcitx::Option<std::string> proxy{this, "Proxy", _("Proxy URL"), ""};
    fcitx::HiddenOption<std::string> liveWsUrl{this, "LiveWsUrl",
                                               _("Live WebSocket URL"), ""};
    fcitx::HiddenOption<std::string> apiBaseUrl{this, "ApiBaseUrl",
                                                _("API Base URL"), ""};);

FCITX_CONFIGURATION(
    GeminiVoiceConfig,
    fcitx::KeyListOption triggerKey{
        this,
        "TriggerKey",
        _("Trigger Key"),
        {fcitx::Key("Alt+space")},
        fcitx::KeyListConstrain({fcitx::KeyConstrainFlag::AllowModifierLess,
                                 fcitx::KeyConstrainFlag::AllowModifierOnly})};
    fcitx::Option<bool> showActionMenu{this, "ShowActionMenu",
                                       _("Show LLM Action Menu"), true};
    fcitx::Option<std::string> socketPath{this, "SocketPath",
                                          _("Daemon Unix Socket Path"),
                                          "$XDG_RUNTIME_DIR/gemini-ime.sock"};
    fcitx::Option<std::string> language{this, "Language",
                                        _("Transcription Language"), ""};
    fcitx::Option<std::string> recordingPrefix{this, "RecordingPrefix",
                                               _("Recording Prefix"), "🎙️"};
    fcitx::Option<int, fcitx::IntConstrain> preeditMaxWidth{
        this, "PreeditMaxWidth", _("Preedit Line Wrap Width"), 80,
        fcitx::IntConstrain(0, 500)};
    fcitx::Option<std::vector<std::string>> translateLanguages{
        this, "TranslateLanguages", _("Translation Target Languages"), {}};
    fcitx::Option<LiveTranscriptionConfig> live{this, "LiveTranscription",
                                                _("Live Transcription")};
    fcitx::Option<PostProcessConfig> postProcess{this, "PostProcess",
                                                 _("Post-processing")};
    fcitx::Option<ConnectionConfig> connection{this, "Connection",
                                               _("Connection")};);

class GeminiVoiceEngine : public fcitx::AddonInstance {
public:
    enum class State {
        IDLE,
        RECORDING,
        STOPPING,
        ACTION_MENU,
        SUBMENU_REFINE,
        SUBMENU_TRANSLATE,
        PROCESSING,
    };

    explicit GeminiVoiceEngine(fcitx::Instance *instance);
    ~GeminiVoiceEngine() override;

    const fcitx::Configuration *getConfig() const override { return &config_; }
    void setConfig(const fcitx::RawConfig &config) override;
    void reloadConfig() override;

    State state() const { return state_; }
    const GeminiVoiceConfig &config() const { return config_; }

private:
    std::string resolveSocketPath() const;
    void onDaemonMessage(const std::string &line);
    void onDaemonDisconnect();
    void onKeyEvent(fcitx::KeyEvent &keyEvent);

    void handleIdleKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleRecordingKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleStoppingKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleActionMenuKey(fcitx::InputContext *ic,
                             fcitx::KeyEvent &keyEvent);
    void handleSubmenuKey(fcitx::InputContext *ic, fcitx::KeyEvent &keyEvent);
    void handleProcessingKey(fcitx::InputContext *ic,
                             fcitx::KeyEvent &keyEvent);

    bool handleCandidateMenuKey(fcitx::InputContext *ic,
                                fcitx::CandidateList &candList,
                                const fcitx::Key &key);
    void setPanelDisplay(fcitx::InputContext *ic, const std::string &auxUp,
                         const std::string &preeditText);
    void applyMenuPanel(fcitx::InputContext *ic, const std::string &auxUp,
                        const std::string &rawText,
                        std::unique_ptr<fcitx::CommonCandidateList> list);
    void clearSession(fcitx::InputContext *ic);
    void showActionMenu(fcitx::InputContext *ic);
    void showRefineSubmenu(fcitx::InputContext *ic);
    void showTranslateSubmenu(fcitx::InputContext *ic);
    std::vector<TranslateTarget> effectiveTranslateTargets() const;
    void triggerPostProcess(fcitx::InputContext *ic, const std::string &action,
                            const std::string &targetLabel = "");
    void cancelActiveSession(fcitx::InputContext *ic);

    fcitx::Instance *instance_;
    GeminiVoiceConfig config_;
    std::shared_ptr<std::atomic<bool>> alive_;
    std::unique_ptr<SocketClient> socketClient_;
    std::vector<std::unique_ptr<fcitx::HandlerTableEntry<fcitx::EventHandler>>>
        eventHandlers_;

    State state_ = State::IDLE;
    fcitx::TrackableObjectReference<fcitx::InputContext> activeIC_;
    std::string partialText_;
    std::string rawText_;
    std::vector<TranslateTarget> translateTargets_;
};

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_ENGINE_H
