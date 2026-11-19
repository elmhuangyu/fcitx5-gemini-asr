#ifndef FCITX5_GEMINI_VOICE_JSON_UTIL_H
#define FCITX5_GEMINI_VOICE_JSON_UTIL_H

#include <initializer_list>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>

namespace fcitx_gemini_voice {

std::string escapeJsonString(std::string_view input);

std::string buildCommandJson(
    std::string_view cmd,
    std::initializer_list<std::pair<std::string_view, std::string_view>>
        fields = {});

// Parses a single-line JSON object into a string map of top-level string
// fields. Non-string values (booleans, numbers, null, nested objects/arrays)
// are validated and skipped. Returns std::nullopt on malformed JSON.
std::optional<std::unordered_map<std::string, std::string>>
parseFlatJsonObject(std::string_view line);

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_JSON_UTIL_H
