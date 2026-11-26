#include "engine_targets.h"

#include <cstdint>
#include <cstdio>
#include <string>
#include <unordered_map>
#include <utility>

namespace fcitx_gemini_voice {

namespace {

std::string trimAsciiWs(std::string_view s) {
    size_t start = 0;
    while (start < s.size() && (s[start] == ' ' || s[start] == '\t' ||
                                s[start] == '\r' || s[start] == '\n')) {
        ++start;
    }
    size_t end = s.size();
    while (end > start && (s[end - 1] == ' ' || s[end - 1] == '\t' ||
                           s[end - 1] == '\r' || s[end - 1] == '\n')) {
        --end;
    }
    return std::string(s.substr(start, end - start));
}

std::string normalizeAliasKey(std::string_view s) {
    std::string out;
    out.reserve(s.size());
    for (char ch : s) {
        if (ch == '_' || ch == ' ') {
            out.push_back('-');
        } else if (ch >= 'A' && ch <= 'Z') {
            out.push_back(static_cast<char>(ch - 'A' + 'a'));
        } else {
            out.push_back(ch);
        }
    }
    return out;
}

std::string slugifyTarget(std::string_view s) {
    std::string out;
    bool prevUnderscore = false;
    for (unsigned char uc : s) {
        char ch = static_cast<char>(uc);
        if (ch >= 'A' && ch <= 'Z') {
            ch = static_cast<char>(ch - 'A' + 'a');
        }
        if ((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
            out.push_back(ch);
            prevUnderscore = false;
        } else if (!prevUnderscore && !out.empty()) {
            out.push_back('_');
            prevUnderscore = true;
        }
    }
    while (!out.empty() && out.back() == '_') {
        out.pop_back();
    }
    if (out.empty()) {
        uint32_t h = 2166136261u;
        for (unsigned char uc : s) {
            h ^= uc;
            h *= 16777619u;
        }
        char buf[16];
        std::snprintf(buf, sizeof(buf), "u_%08x", h);
        return std::string(buf);
    }
    return out;
}
} // namespace

std::optional<TranslateTarget> resolveTranslateTarget(std::string_view raw) {
    std::string trimmed = trimAsciiWs(raw);
    if (trimmed.empty()) {
        return std::nullopt;
    }
    static const std::unordered_map<std::string, TranslateTarget> known = {
        {"en", {"translate_en", "English"}},
        {"english", {"translate_en", "English"}},
        {"zh", {"translate_zh_cn", "Chinese (Simplified)"}},
        {"zh-cn", {"translate_zh_cn", "Chinese (Simplified)"}},
        {"zh-hans", {"translate_zh_cn", "Chinese (Simplified)"}},
        {"chinese", {"translate_zh_cn", "Chinese (Simplified)"}},
        {"simplified-chinese", {"translate_zh_cn", "Chinese (Simplified)"}},
        {"zh-tw", {"translate_zh_tw", "Chinese (Traditional)"}},
        {"zh-hant", {"translate_zh_tw", "Chinese (Traditional)"}},
        {"traditional-chinese", {"translate_zh_tw", "Chinese (Traditional)"}},
        {"ja", {"translate_ja", "Japanese"}},
        {"jp", {"translate_ja", "Japanese"}},
        {"japanese", {"translate_ja", "Japanese"}},
        {"ko", {"translate_ko", "Korean"}},
        {"korean", {"translate_ko", "Korean"}},
        {"fr", {"translate_fr", "French"}},
        {"french", {"translate_fr", "French"}},
        {"de", {"translate_de", "German"}},
        {"german", {"translate_de", "German"}},
        {"es", {"translate_es", "Spanish"}},
        {"spanish", {"translate_es", "Spanish"}},
        {"pt", {"translate_pt", "Portuguese"}},
        {"portuguese", {"translate_pt", "Portuguese"}},
        {"ru", {"translate_ru", "Russian"}},
        {"russian", {"translate_ru", "Russian"}},
        {"it", {"translate_it", "Italian"}},
        {"italian", {"translate_it", "Italian"}},
        {"ar", {"translate_ar", "Arabic"}},
        {"arabic", {"translate_ar", "Arabic"}},
        {"vi", {"translate_vi", "Vietnamese"}},
        {"vietnamese", {"translate_vi", "Vietnamese"}},
        {"th", {"translate_th", "Thai"}},
        {"thai", {"translate_th", "Thai"}},
        {"id", {"translate_id", "Indonesian"}},
        {"indonesian", {"translate_id", "Indonesian"}},
    };
    auto it = known.find(normalizeAliasKey(trimmed));
    if (it != known.end()) {
        return it->second;
    }
    return TranslateTarget{"translate_" + slugifyTarget(trimmed), trimmed};
}

std::vector<TranslateTarget>
resolveConfiguredTargets(const std::vector<std::string> &langs) {
    std::vector<TranslateTarget> out;
    for (const auto &raw : langs) {
        auto resolved = resolveTranslateTarget(raw);
        if (!resolved.has_value()) {
            continue;
        }
        bool dup = false;
        for (const auto &existing : out) {
            if (existing.action == resolved->action) {
                dup = true;
                break;
            }
        }
        if (!dup) {
            out.push_back(std::move(*resolved));
        }
    }
    return out;
}

} // namespace fcitx_gemini_voice
