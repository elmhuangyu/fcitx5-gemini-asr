#include "json_util.h"

#include <nlohmann/json.hpp>

namespace fcitx_gemini_voice {

namespace {

using nlohmann::json;
using nlohmann::ordered_json;

// Non-throwing parse; the whole input must be a single JSON value.
json parseLine(std::string_view line) {
    return json::parse(line.begin(), line.end(), nullptr, false);
}

std::string dumpReplacing(const ordered_json &j) {
    return j.dump(-1, ' ', false, ordered_json::error_handler_t::replace);
}

} // namespace

std::string escapeJsonString(std::string_view input) {
    std::string quoted = dumpReplacing(ordered_json(std::string(input)));
    return quoted.substr(1, quoted.size() - 2);
}

std::string buildCommandJson(
    std::string_view cmd,
    std::initializer_list<std::pair<std::string_view, std::string_view>>
        fields) {
    ordered_json obj = ordered_json::object();
    obj["cmd"] = std::string(cmd);
    for (const auto &kv : fields) {
        if (kv.second.empty()) {
            continue;
        }
        obj[std::string(kv.first)] = std::string(kv.second);
    }
    return dumpReplacing(obj);
}

std::optional<std::unordered_map<std::string, std::string>>
parseFlatJsonObject(std::string_view line) {
    json doc = parseLine(line);
    if (!doc.is_object()) {
        return std::nullopt;
    }
    std::unordered_map<std::string, std::string> result;
    for (auto it = doc.begin(); it != doc.end(); ++it) {
        if (it.value().is_string()) {
            result.insert_or_assign(it.key(), it.value().get<std::string>());
        }
    }
    return result;
}

std::vector<TranslateTarget> parseTranslateTargets(std::string_view line) {
    json doc = parseLine(line);
    if (!doc.is_object()) {
        return {};
    }
    auto found = doc.find("translate_targets");
    if (found == doc.end() || !found->is_array()) {
        return {};
    }
    std::vector<TranslateTarget> targets;
    for (const auto &elem : *found) {
        if (!elem.is_object()) {
            return {};
        }
        TranslateTarget item;
        auto action = elem.find("action");
        if (action != elem.end() && action->is_string()) {
            item.action = action->get<std::string>();
        }
        auto label = elem.find("label");
        if (label != elem.end() && label->is_string()) {
            item.label = label->get<std::string>();
        }
        if (!item.action.empty() && !item.label.empty()) {
            targets.push_back(std::move(item));
        }
    }
    return targets;
}

} // namespace fcitx_gemini_voice
