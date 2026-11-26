#ifndef FCITX5_GEMINI_VOICE_ENGINE_TARGETS_H
#define FCITX5_GEMINI_VOICE_ENGINE_TARGETS_H

#include "json_util.h"

#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace fcitx_gemini_voice {

// Map a user-configured language name/alias to a translate action + label.
std::optional<TranslateTarget> resolveTranslateTarget(std::string_view raw);

// Resolve a configured list, dropping blanks and duplicate actions.
std::vector<TranslateTarget>
resolveConfiguredTargets(const std::vector<std::string> &langs);

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_ENGINE_TARGETS_H
