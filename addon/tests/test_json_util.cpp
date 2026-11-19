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

    std::cout << "test_json_util PASSED\n";
    return 0;
}
