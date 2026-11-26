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
#include <fcitx-config/rawconfig.h>
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

// Names of the actions an Actions-list entry can override. The stored value is
// the daemon's action name; the label is shown in the Fcitx5 config UI. The
// option stays a plain string (EnumAnnotation) so a value that is not in this
// list, such as an action for a custom translation language written by the CLI,
// survives loading and saving untouched.
struct ActionNameEntry {
    const char *value;
    const char *label;
};
constexpr ActionNameEntry kActionNames[] = {
    {"", N_("(choose an action)")},
    {"refine_fix_typos", N_("Refine: Fix typos")},
    {"refine_casual", N_("Refine: Casual")},
    {"refine_formal", N_("Refine: Formal")},
    {"as_prompt", N_("As Prompt")},
    {"ask_ai", N_("Ask AI")},
    {"translate_en", N_("Translate: English")},
    {"translate_zh_cn", N_("Translate: Chinese (Simplified)")},
    {"translate_zh_tw", N_("Translate: Chinese (Traditional)")},
    {"translate_ja", N_("Translate: Japanese")},
    {"translate_ko", N_("Translate: Korean")},
    {"translate_fr", N_("Translate: French")},
    {"translate_de", N_("Translate: German")},
    {"translate_es", N_("Translate: Spanish")},
    {"translate_pt", N_("Translate: Portuguese")},
    {"translate_ru", N_("Translate: Russian")},
    {"translate_it", N_("Translate: Italian")},
    {"translate_ar", N_("Translate: Arabic")},
    {"translate_vi", N_("Translate: Vietnamese")},
    {"translate_th", N_("Translate: Thai")},
    {"translate_id", N_("Translate: Indonesian")},
};

struct ActionNameAnnotation : public fcitx::EnumAnnotation {
    void dumpDescription(fcitx::RawConfig &config) const {
        fcitx::EnumAnnotation::dumpDescription(config);
        int i = 0;
        for (const auto &entry : kActionNames) {
            config.setValueByPath("Enum/" + std::to_string(i), entry.value);
            config.setValueByPath("EnumI18n/" + std::to_string(i),
                                  _(entry.label));
            ++i;
        }
    }
};

FCITX_CONFIGURATION(
    ActionOverrideConfig,
    fcitx::Option<std::string, fcitx::NoConstrain<std::string>,
                  fcitx::DefaultMarshaller<std::string>, ActionNameAnnotation>
        name{this, "Name", _("Action"), "", {}, {}, {}};
    fcitx::Option<std::string> prompt{this, "Prompt",
                                      _("Prompt (empty uses built-in)")};
    fcitx::Option<std::string> model{
        this, "Model", _("Model (empty uses default post-process model)")};);

// Built-in prompts, shown as tooltips. They mirror defaultPrompts in the
// daemon (daemon/config.go); a Go test keeps the two in sync.
// clang-format off
#define GV_PROMPT_FIX_TYPOS "仅修正以下语音转录文本中的错别字、同音字错误和标点符号。严禁增删词汇或改变原本的口语语气，直接输出结果："
#define GV_PROMPT_CASUAL "将以下文本润色为自然、流畅、轻松的日常对话语气（适合即时通讯聊天），保留原意，直接输出结果："
#define GV_PROMPT_FORMAL "将以下文本润色为清晰、专业、得体的正式书面语（适合工作邮件或技术文档），直接输出结果："
#define GV_PROMPT_AS_PROMPT "将以下口述需求整理为结构清晰、条件明确的 AI Prompt，直接输出提示词内容："
#define GV_PROMPT_TRANSLATE "Translate the following text into natural, idiomatic <language>. Output ONLY the translation:"
#define GV_PROMPT_ASK_AI "请直接将以下语音内容作为提示词或指令执行，并直接输出生成的最终内容，不要复述指令或添加多余解释："
// clang-format on

using TooltipString =
    fcitx::Option<std::string, fcitx::NoConstrain<std::string>,
                  fcitx::DefaultMarshaller<std::string>,
                  fcitx::ToolTipAnnotation>;

inline std::string promptTooltip(const char *builtin) {
    return std::string(_("Leave empty to use the built-in prompt:")) + "\n" +
           builtin;
}

// One fixed group per built-in action (Translate covers every translate_*
// action): its own model, prompt and prefetch switch.
#define GV_ACTION_GROUP(NAME, DEFAULT_PROMPT, PREFETCH_DEFAULT)                \
    FCITX_CONFIGURATION(                                                       \
        NAME,                                                                  \
        TooltipString model{                                                   \
            this,                                                              \
            "Model",                                                           \
            _("Model (empty uses default model)"),                             \
            "",                                                                \
            {},                                                                \
            {},                                                                \
            fcitx::ToolTipAnnotation(                                          \
                _("Model name, optionally followed by a thinking effort "      \
                  "suffix: model/low, model/medium or model/high (gemini-3 "   \
                  "and newer). Example: gemini-3.5-flash/low."))};             \
        TooltipString prompt{                                                  \
            this,                                                              \
            "Prompt",                                                          \
            _("Prompt (empty uses built-in)"),                                 \
            "",                                                                \
            {},                                                                \
            {},                                                                \
            fcitx::ToolTipAnnotation(promptTooltip(DEFAULT_PROMPT))};          \
        fcitx::Option<bool> prefetch{this, "Prefetch",                         \
                                     _("Run when recording stops (prefetch)"), \
                                     PREFETCH_DEFAULT};)

GV_ACTION_GROUP(RefineFixTyposConfig, GV_PROMPT_FIX_TYPOS, true)
GV_ACTION_GROUP(RefineCasualConfig, GV_PROMPT_CASUAL, false)
GV_ACTION_GROUP(RefineFormalConfig, GV_PROMPT_FORMAL, true)
GV_ACTION_GROUP(AsPromptConfig, GV_PROMPT_AS_PROMPT, false)
GV_ACTION_GROUP(TranslateConfig, GV_PROMPT_TRANSLATE, false)
GV_ACTION_GROUP(AskAiConfig, GV_PROMPT_ASK_AI, false)

FCITX_CONFIGURATION(
    PostProcessConfig,
    fcitx::Option<std::string> model{this, "Model", _("Default Model"),
                                     "gemini-3.5-flash-lite"};
    fcitx::Option<RefineFixTyposConfig> refineFixTypos{this, "RefineFixTypos",
                                                       _("Refine: Fix Typos")};
    fcitx::Option<RefineCasualConfig> refineCasual{this, "RefineCasual",
                                                   _("Refine: Casual")};
    fcitx::Option<RefineFormalConfig> refineFormal{this, "RefineFormal",
                                                   _("Refine: Formal")};
    fcitx::Option<AsPromptConfig> asPrompt{this, "AsPrompt", _("As Prompt")};
    fcitx::Option<TranslateConfig> translate{
        this, "Translate", _("Translate (all target languages)")};
    fcitx::Option<AskAiConfig> askAi{this, "AskAi", _("Ask AI")};
    fcitx::Option<std::vector<ActionOverrideConfig>,
                  fcitx::NoConstrain<std::vector<ActionOverrideConfig>>,
                  fcitx::DefaultMarshaller<std::vector<ActionOverrideConfig>>,
                  fcitx::ListDisplayOptionAnnotation>
        actions{this,
                "Actions",
                _("Per-action Overrides (advanced; take precedence over the "
                  "groups above)"),
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
