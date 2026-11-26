#include "engine_text.h"

#include <cstdint>

#include <fcitx-utils/utf8.h>

namespace fcitx_gemini_voice {

std::string truncateUtf8(const std::string &str, size_t maxChars) {
    if (!fcitx::utf8::validate(str)) {
        return str.size() > maxChars ? str.substr(0, maxChars) + "..." : str;
    }
    if (fcitx::utf8::length(str) <= maxChars) {
        return str;
    }
    ssize_t bytes = fcitx::utf8::ncharByteLength(str.begin(), maxChars);
    if (bytes < 0) {
        return str;
    }
    return str.substr(0, static_cast<size_t>(bytes)) + "...";
}

namespace {

int charDisplayWidth(uint32_t cp) {
    if (cp == 0) {
        return 0;
    }
    if ((cp >= 0x1100 && cp <= 0x115F) || cp == 0x2329 || cp == 0x232A ||
        (cp >= 0x2E80 && cp <= 0x303E) || (cp >= 0x3040 && cp <= 0x3247) ||
        (cp >= 0x3250 && cp <= 0x4DBF) || (cp >= 0x4E00 && cp <= 0xA4CF) ||
        (cp >= 0xA960 && cp <= 0xA97F) || (cp >= 0xAC00 && cp <= 0xD7A3) ||
        (cp >= 0xF900 && cp <= 0xFAFF) || (cp >= 0xFE10 && cp <= 0xFE19) ||
        (cp >= 0xFE30 && cp <= 0xFE6F) || (cp >= 0xFF01 && cp <= 0xFF60) ||
        (cp >= 0xFFE0 && cp <= 0xFFE6) || (cp >= 0x1F300 && cp <= 0x1FAFF) ||
        (cp >= 0x20000 && cp <= 0x3FFFD)) {
        return 2;
    }
    return 1;
}
} // namespace

std::string wrapPreeditText(const std::string &text, int maxWidth) {
    if (maxWidth <= 0 || text.empty() || !fcitx::utf8::validate(text)) {
        return text;
    }

    std::string out;
    out.reserve(text.size() + 8);
    int lineWidth = 0;
    size_t lineStartOutIdx = 0;
    size_t lastSpaceOutIdx = 0;
    int widthSinceSpace = 0;

    auto it = text.begin();
    const auto end = text.end();
    while (it != end) {
        uint32_t cp = 0;
        auto next = fcitx::utf8::getNextChar(it, end, &cp);
        if (!fcitx::utf8::isValidChar(cp)) {
            out.append(it, next);
            it = next;
            continue;
        }
        if (cp == '\n') {
            out.push_back('\n');
            lineWidth = 0;
            lineStartOutIdx = out.size();
            lastSpaceOutIdx = 0;
            widthSinceSpace = 0;
            it = next;
            continue;
        }

        const int w = charDisplayWidth(cp);
        if (lineWidth > 0 && lineWidth + w > maxWidth) {
            if (cp == ' ') {
                out.push_back('\n');
                lineWidth = 0;
                lineStartOutIdx = out.size();
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
                it = next;
                continue;
            }
            if (lastSpaceOutIdx > lineStartOutIdx &&
                widthSinceSpace <= maxWidth / 3 && cp < 0x80) {
                out[lastSpaceOutIdx - 1] = '\n';
                lineStartOutIdx = lastSpaceOutIdx;
                lineWidth = widthSinceSpace;
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
            } else {
                out.push_back('\n');
                lineWidth = 0;
                lineStartOutIdx = out.size();
                lastSpaceOutIdx = 0;
                widthSinceSpace = 0;
            }
        }

        out.append(it, next);
        lineWidth += w;
        if (cp == ' ') {
            lastSpaceOutIdx = out.size();
            widthSinceSpace = 0;
        } else if (lastSpaceOutIdx > lineStartOutIdx) {
            widthSinceSpace += w;
        }
        it = next;
    }
    return out;
}

} // namespace fcitx_gemini_voice
