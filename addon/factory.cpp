#include "engine.h"

#include <fcitx/addonfactory.h>
#include <fcitx/addonmanager.h>

namespace fcitx_gemini_voice {

class GeminiVoiceEngineFactory : public fcitx::AddonFactory {
public:
    fcitx::AddonInstance *create(fcitx::AddonManager *manager) override {
        return new GeminiVoiceEngine(manager->instance());
    }
};

} // namespace fcitx_gemini_voice

FCITX_ADDON_FACTORY(fcitx_gemini_voice::GeminiVoiceEngineFactory)
