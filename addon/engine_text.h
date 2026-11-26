#ifndef FCITX5_GEMINI_VOICE_ENGINE_TEXT_H
#define FCITX5_GEMINI_VOICE_ENGINE_TEXT_H

#include <cstddef>
#include <string>

namespace fcitx_gemini_voice {

// Truncate to at most maxChars UTF-8 characters, appending "..." if cut.
std::string truncateUtf8(const std::string &str, size_t maxChars);

// Wrap text to maxWidth display columns (CJK counted as double width).
std::string wrapPreeditText(const std::string &text, int maxWidth);

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_ENGINE_TEXT_H
