#ifndef FCITX5_GEMINI_VOICE_JSON_UTIL_H
#define FCITX5_GEMINI_VOICE_JSON_UTIL_H

#include <initializer_list>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>
#include <vector>

namespace fcitx_gemini_voice {

struct TranslateTarget {
    std::string action;
    std::string label;
};

std::string escapeJsonString(std::string_view input);

std::string buildCommandJson(
    std::string_view cmd,
    std::initializer_list<std::pair<std::string_view, std::string_view>>
        fields = {});

// Parses (via nlohmann/json) a single-line JSON object into a string map of
// top-level string fields. Non-string values (booleans, numbers, null, nested
// objects/arrays) are validated and skipped. Returns std::nullopt on malformed
// JSON.
std::optional<std::unordered_map<std::string, std::string>>
parseFlatJsonObject(std::string_view line);

// Extracts the "translate_targets" array of {action, label} objects from a
// single-line JSON object. Returns empty on absence or malformed input.
std::vector<TranslateTarget> parseTranslateTargets(std::string_view line);

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_JSON_UTIL_H
