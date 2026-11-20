#include "json_util.h"

#include <cstdint>
#include <cstdio>

namespace fcitx_gemini_voice {

namespace {

void appendUtf8(std::string &out, uint32_t cp) {
    if (cp <= 0x7F) {
        out.push_back(static_cast<char>(cp));
    } else if (cp <= 0x7FF) {
        out.push_back(static_cast<char>(0xC0 | ((cp >> 6) & 0x1F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp <= 0xFFFF) {
        out.push_back(static_cast<char>(0xE0 | ((cp >> 12) & 0x0F)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp <= 0x10FFFF) {
        out.push_back(static_cast<char>(0xF0 | ((cp >> 18) & 0x07)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
}

class JsonCursor {
public:
    explicit JsonCursor(std::string_view input) : s_(input), pos_(0) {}

    void skipWs() {
        while (pos_ < s_.size()) {
            char c = s_[pos_];
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                ++pos_;
            } else {
                break;
            }
        }
    }

    bool eof() const { return pos_ >= s_.size(); }

    char peek() const { return pos_ < s_.size() ? s_[pos_] : '\0'; }

    bool consume(char expected) {
        if (pos_ < s_.size() && s_[pos_] == expected) {
            ++pos_;
            return true;
        }
        return false;
    }

    std::optional<std::string> parseString() {
        if (!consume('"')) {
            return std::nullopt;
        }
        std::string out;
        while (pos_ < s_.size()) {
            unsigned char c = static_cast<unsigned char>(s_[pos_++]);
            if (c == '"') {
                return out;
            }
            if (c < 0x20) {
                return std::nullopt;
            }
            if (c != '\\') {
                out.push_back(static_cast<char>(c));
                continue;
            }
            if (pos_ >= s_.size()) {
                return std::nullopt;
            }
            char esc = s_[pos_++];
            switch (esc) {
            case '"':
                out.push_back('"');
                break;
            case '\\':
                out.push_back('\\');
                break;
            case '/':
                out.push_back('/');
                break;
            case 'b':
                out.push_back('\b');
                break;
            case 'f':
                out.push_back('\f');
                break;
            case 'n':
                out.push_back('\n');
                break;
            case 'r':
                out.push_back('\r');
                break;
            case 't':
                out.push_back('\t');
                break;
            case 'u': {
                uint32_t u1 = 0;
                if (!parseHex4(u1)) {
                    return std::nullopt;
                }
                if (u1 >= 0xD800 && u1 <= 0xDBFF) {
                    if (pos_ + 6 > s_.size() || s_[pos_] != '\\' ||
                        s_[pos_ + 1] != 'u') {
                        return std::nullopt;
                    }
                    pos_ += 2;
                    uint32_t u2 = 0;
                    if (!parseHex4(u2) || u2 < 0xDC00 || u2 > 0xDFFF) {
                        return std::nullopt;
                    }
                    uint32_t cp =
                        0x10000 + (((u1 - 0xD800) << 10) | (u2 - 0xDC00));
                    appendUtf8(out, cp);
                } else if (u1 >= 0xDC00 && u1 <= 0xDFFF) {
                    return std::nullopt;
                } else {
                    appendUtf8(out, u1);
                }
                break;
            }
            default:
                return std::nullopt;
            }
        }
        return std::nullopt;
    }

    bool skipValue(int depth = 0) {
        if (depth > 64) {
            return false;
        }
        skipWs();
        if (eof()) {
            return false;
        }
        char c = peek();
        if (c == '"') {
            return parseString().has_value();
        }
        if (c == '{') {
            ++pos_;
            skipWs();
            if (consume('}')) {
                return true;
            }
            while (true) {
                skipWs();
                if (!parseString().has_value()) {
                    return false;
                }
                skipWs();
                if (!consume(':')) {
                    return false;
                }
                if (!skipValue(depth + 1)) {
                    return false;
                }
                skipWs();
                if (consume('}')) {
                    return true;
                }
                if (!consume(',')) {
                    return false;
                }
            }
        }
        if (c == '[') {
            ++pos_;
            skipWs();
            if (consume(']')) {
                return true;
            }
            while (true) {
                if (!skipValue(depth + 1)) {
                    return false;
                }
                skipWs();
                if (consume(']')) {
                    return true;
                }
                if (!consume(',')) {
                    return false;
                }
            }
        }
        if (matchLiteral("true") || matchLiteral("false") ||
            matchLiteral("null")) {
            return true;
        }
        return skipNumber();
    }

private:
    bool parseHex4(uint32_t &out) {
        if (pos_ + 4 > s_.size()) {
            return false;
        }
        uint32_t val = 0;
        for (int i = 0; i < 4; ++i) {
            char c = s_[pos_++];
            val <<= 4;
            if (c >= '0' && c <= '9') {
                val |= static_cast<uint32_t>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                val |= static_cast<uint32_t>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                val |= static_cast<uint32_t>(c - 'A' + 10);
            } else {
                return false;
            }
        }
        out = val;
        return true;
    }

    bool matchLiteral(std::string_view lit) {
        if (s_.substr(pos_, lit.size()) == lit) {
            pos_ += lit.size();
            return true;
        }
        return false;
    }

    bool skipNumber() {
        size_t start = pos_;
        if (pos_ < s_.size() && s_[pos_] == '-') {
            ++pos_;
        }
        if (pos_ >= s_.size()) {
            pos_ = start;
            return false;
        }
        if (s_[pos_] == '0') {
            ++pos_;
        } else if (s_[pos_] >= '1' && s_[pos_] <= '9') {
            while (pos_ < s_.size() && s_[pos_] >= '0' && s_[pos_] <= '9') {
                ++pos_;
            }
        } else {
            pos_ = start;
            return false;
        }
        if (pos_ < s_.size() && s_[pos_] == '.') {
            ++pos_;
            if (pos_ >= s_.size() || s_[pos_] < '0' || s_[pos_] > '9') {
                pos_ = start;
                return false;
            }
            while (pos_ < s_.size() && s_[pos_] >= '0' && s_[pos_] <= '9') {
                ++pos_;
            }
        }
        if (pos_ < s_.size() && (s_[pos_] == 'e' || s_[pos_] == 'E')) {
            ++pos_;
            if (pos_ < s_.size() && (s_[pos_] == '+' || s_[pos_] == '-')) {
                ++pos_;
            }
            if (pos_ >= s_.size() || s_[pos_] < '0' || s_[pos_] > '9') {
                pos_ = start;
                return false;
            }
            while (pos_ < s_.size() && s_[pos_] >= '0' && s_[pos_] <= '9') {
                ++pos_;
            }
        }
        return pos_ > start;
    }

    std::string_view s_;
    size_t pos_;
};

} // namespace

std::string escapeJsonString(std::string_view input) {
    std::string out;
    out.reserve(input.size() + 8);
    for (unsigned char c : input) {
        switch (c) {
        case '"':
            out += "\\\"";
            break;
        case '\\':
            out += "\\\\";
            break;
        case '\b':
            out += "\\b";
            break;
        case '\f':
            out += "\\f";
            break;
        case '\n':
            out += "\\n";
            break;
        case '\r':
            out += "\\r";
            break;
        case '\t':
            out += "\\t";
            break;
        default:
            if (c < 0x20) {
                char buf[7];
                std::snprintf(buf, sizeof(buf), "\\u%04x",
                              static_cast<unsigned int>(c));
                out += buf;
            } else {
                out.push_back(static_cast<char>(c));
            }
            break;
        }
    }
    return out;
}

std::string buildCommandJson(
    std::string_view cmd,
    std::initializer_list<std::pair<std::string_view, std::string_view>>
        fields) {
    std::string out = "{\"cmd\":\"";
    out += escapeJsonString(cmd);
    out += '"';
    for (const auto &kv : fields) {
        if (kv.second.empty()) {
            continue;
        }
        out += ",\"";
        out += escapeJsonString(kv.first);
        out += "\":\"";
        out += escapeJsonString(kv.second);
        out += '"';
    }
    out += '}';
    return out;
}

std::optional<std::unordered_map<std::string, std::string>>
parseFlatJsonObject(std::string_view line) {
    JsonCursor cur(line);
    cur.skipWs();
    if (!cur.consume('{')) {
        return std::nullopt;
    }

    std::unordered_map<std::string, std::string> result;
    cur.skipWs();
    if (cur.consume('}')) {
        cur.skipWs();
        if (!cur.eof()) {
            return std::nullopt;
        }
        return result;
    }

    while (true) {
        cur.skipWs();
        auto key = cur.parseString();
        if (!key.has_value()) {
            return std::nullopt;
        }
        cur.skipWs();
        if (!cur.consume(':')) {
            return std::nullopt;
        }
        cur.skipWs();
        if (cur.peek() == '"') {
            auto val = cur.parseString();
            if (!val.has_value()) {
                return std::nullopt;
            }
            result.insert_or_assign(std::move(*key), std::move(*val));
        } else {
            if (!cur.skipValue()) {
                return std::nullopt;
            }
        }
        cur.skipWs();
        if (cur.consume('}')) {
            break;
        }
        if (!cur.consume(',')) {
            return std::nullopt;
        }
    }

    cur.skipWs();
    if (!cur.eof()) {
        return std::nullopt;
    }
    return result;
}

std::vector<TranslateTarget> parseTranslateTargets(std::string_view line) {
    JsonCursor cur(line);
    cur.skipWs();
    if (!cur.consume('{')) {
        return {};
    }
    cur.skipWs();
    if (cur.consume('}')) {
        return {};
    }

    std::vector<TranslateTarget> targets;
    while (true) {
        cur.skipWs();
        auto key = cur.parseString();
        if (!key.has_value()) {
            return {};
        }
        cur.skipWs();
        if (!cur.consume(':')) {
            return {};
        }
        cur.skipWs();
        if (*key == "translate_targets" && cur.peek() == '[') {
            cur.consume('[');
            cur.skipWs();
            if (!cur.consume(']')) {
                while (true) {
                    cur.skipWs();
                    if (!cur.consume('{')) {
                        return {};
                    }
                    TranslateTarget item;
                    cur.skipWs();
                    if (!cur.consume('}')) {
                        while (true) {
                            cur.skipWs();
                            auto fk = cur.parseString();
                            if (!fk.has_value()) {
                                return {};
                            }
                            cur.skipWs();
                            if (!cur.consume(':')) {
                                return {};
                            }
                            cur.skipWs();
                            if (cur.peek() == '"') {
                                auto fv = cur.parseString();
                                if (!fv.has_value()) {
                                    return {};
                                }
                                if (*fk == "action") {
                                    item.action = std::move(*fv);
                                } else if (*fk == "label") {
                                    item.label = std::move(*fv);
                                }
                            } else if (!cur.skipValue()) {
                                return {};
                            }
                            cur.skipWs();
                            if (cur.consume('}')) {
                                break;
                            }
                            if (!cur.consume(',')) {
                                return {};
                            }
                        }
                    }
                    if (!item.action.empty() && !item.label.empty()) {
                        targets.push_back(std::move(item));
                    }
                    cur.skipWs();
                    if (cur.consume(']')) {
                        break;
                    }
                    if (!cur.consume(',')) {
                        return {};
                    }
                }
            }
        } else if (!cur.skipValue()) {
            return {};
        }
        cur.skipWs();
        if (cur.consume('}')) {
            break;
        }
        if (!cur.consume(',')) {
            return {};
        }
    }
    return targets;
}

} // namespace fcitx_gemini_voice
