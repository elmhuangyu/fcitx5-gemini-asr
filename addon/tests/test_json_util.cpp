#include "json_util.h"

#include <cstdlib>
#include <iostream>
#include <string>

#define REQUIRE(cond)                                                          \
    do {                                                                       \
        if (!(cond)) {                                                         \
            std::cerr << "FAIL at " << __FILE__ << ":" << __LINE__ << ": "     \
                      << #cond << "\n";                                        \
            std::abort();                                                      \
        }                                                                      \
    } while (0)

using fcitx_gemini_voice::buildCommandJson;
using fcitx_gemini_voice::escapeJsonString;
using fcitx_gemini_voice::parseFlatJsonObject;
using fcitx_gemini_voice::parseTranslateTargets;

int main() {
    // 1. Escaping and command encoding
    REQUIRE(escapeJsonString("hello\n\"world\"\\a\t") ==
            "hello\\n\\\"world\\\"\\\\a\\t");
    REQUIRE(escapeJsonString("中文测试") == "中文测试");

    std::string cmd1 =
        buildCommandJson("start_recording", {{"language", "zh-CN"}});
    REQUIRE(cmd1 == "{\"cmd\":\"start_recording\",\"language\":\"zh-CN\"}");

    std::string cmd2 = buildCommandJson("start_recording", {{"language", ""}});
    REQUIRE(cmd2 == "{\"cmd\":\"start_recording\"}");

    std::string cmd3 =
        buildCommandJson("post_process", {{"action", "refine_formal"}});
    REQUIRE(cmd3 == "{\"cmd\":\"post_process\",\"action\":\"refine_formal\"}");

    // 2. Parsing valid NDJSON events (UTF-8, escapes, unicode & surrogate
    // pairs)
    {
        auto obj = parseFlatJsonObject(
            R"({"event":"partial_text","text":"今天\"下午\"\n开会\u4e2d\uD83D\uDE00","seq":42,"ok":true,"meta":{"a":[1,2]}})");
        REQUIRE(obj.has_value());
        REQUIRE((*obj)["event"] == "partial_text");
        REQUIRE((*obj)["text"] == "今天\"下午\"\n开会中😀");
    }

    // 3. Empty object
    {
        auto obj = parseFlatJsonObject(" { } ");
        REQUIRE(obj.has_value());
        REQUIRE(obj->empty());
    }

    // 4. Reject malformed JSON
    REQUIRE(!parseFlatJsonObject("").has_value());
    REQUIRE(!parseFlatJsonObject("[]").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"x\"").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"x\",}").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"x\"} trailing").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"unterminated").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"\x01\"}").has_value());
    REQUIRE(!parseFlatJsonObject("{\"event\":\"\\uD800\"}").has_value());

    // 5. Parse translate_targets array
    {
        auto targets = parseTranslateTargets(
            R"({"event":"transcribe_done","raw_text":"hi","translate_targets":[{"action":"translate_en","label":"English"},{"action":"translate_ja","label":"Japanese"}]})");
        REQUIRE(targets.size() == 2);
        REQUIRE(targets[0].action == "translate_en");
        REQUIRE(targets[0].label == "English");
        REQUIRE(targets[1].action == "translate_ja");
        REQUIRE(targets[1].label == "Japanese");
    }
    REQUIRE(
        parseTranslateTargets(R"({"event":"transcribe_done","raw_text":"hi"})")
            .empty());

    // 6. Edge cases around the nlohmann/json backend
    {
        // Control chars are escaped as lowercase \u00xx.
        REQUIRE(escapeJsonString(std::string("a\x01\x1f", 3)) ==
                "a\\u0001\\u001f");
        // Invalid UTF-8 is replaced with U+FFFD rather than throwing.
        REQUIRE(escapeJsonString("a\xff") == "a\xEF\xBF\xBD");
        REQUIRE(buildCommandJson("x", {{"k", "\xff"}}) ==
                "{\"cmd\":\"x\",\"k\":\"\xEF\xBF\xBD\"}");
        // Duplicate keys: last wins.
        auto dup = parseFlatJsonObject(R"({"a":"1","a":"2"})");
        REQUIRE(dup.has_value() && (*dup)["a"] == "2");
        // Non-string values are skipped, not errors.
        auto skip = parseFlatJsonObject(R"({"n":1,"z":null,"o":{},"s":"v"})");
        REQUIRE(skip.has_value() && skip->size() == 1);
        // Invalid UTF-8 in input is rejected.
        REQUIRE(!parseFlatJsonObject("{\"a\":\"\xff\"}").has_value());
        // Malformed numbers rejected.
        REQUIRE(!parseFlatJsonObject(R"({"n":01})").has_value());
        // translate_targets: bad element aborts, incomplete items skipped.
        REQUIRE(parseTranslateTargets(R"({"translate_targets":[1]})").empty());
        auto tt = parseTranslateTargets(
            R"({"translate_targets":[{"action":"a"},{"action":"b","label":"B","x":1}]})");
        REQUIRE(tt.size() == 1 && tt[0].action == "b");
        REQUIRE(parseTranslateTargets(R"({"translate_targets":"s"})").empty());
        REQUIRE(parseTranslateTargets("{bad").empty());
    }

    std::cout << "test_json_util PASSED\n";
    return 0;
}
