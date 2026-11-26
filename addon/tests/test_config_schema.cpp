// Verifies the on-disk serialization of GeminiVoiceConfig, the single source of
// truth shared with the Go daemon. The daemon's loader tests parse the golden
// files this test checks, so both sides agree on real Fcitx5 INI output.
//
// Rerun with UPDATE_GOLDEN=1 to regenerate the golden files. (gemini-voice.conf
// is produced by test_engine_integration through the real setConfig path.)

#include "engine.h"

#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>

#include <fcitx-config/iniparser.h>
#include <fcitx-config/rawconfig.h>

#define REQUIRE(cond)                                                          \
    do {                                                                       \
        if (!(cond)) {                                                         \
            std::cerr << "REQUIRE failed: " #cond " at line " << __LINE__      \
                      << std::endl;                                            \
            std::exit(1);                                                      \
        }                                                                      \
    } while (0)

namespace {

std::string slurp(const std::string &path) {
    std::ifstream in(path, std::ios::binary);
    REQUIRE(in.good());
    std::stringstream ss;
    ss << in.rdbuf();
    return ss.str();
}

std::string render(const fcitx_gemini_voice::GeminiVoiceConfig &cfg) {
    std::stringstream ss;
    fcitx::RawConfig raw;
    cfg.save(raw);
    fcitx::writeAsIni(raw, ss);
    return ss.str();
}

void check(const std::string &name, const std::string &actual) {
    const std::string path = std::string(GOLDEN_DIR) + "/" + name;
    if (std::getenv("UPDATE_GOLDEN")) {
        std::ofstream(path, std::ios::binary) << actual;
    }
    const std::string expected = slurp(path);
    if (actual != expected) {
        std::cerr << "golden mismatch for " << name
                  << " (rerun with UPDATE_GOLDEN=1)\n--- actual\n"
                  << actual << "--- golden\n"
                  << expected;
    }
    REQUIRE(actual == expected);
}

using fcitx_gemini_voice::ActionOverrideConfig;
using fcitx_gemini_voice::GeminiVoiceConfig;

} // namespace

int main() {
    ::setenv("LC_ALL", "C", 1);
    ::setenv("LANGUAGE", "C", 1);

    // All defaults: Fcitx5 writes default-valued options as commented lines.
    {
        GeminiVoiceConfig cfg;
        check("gemini-voice.defaults.conf", render(cfg));
    }

    // Prefetch switched off everywhere, and an auto-stop of 0.
    {
        GeminiVoiceConfig cfg;
        auto *pp = cfg.postProcess.mutableValue();
        pp->refineFixTypos.mutableValue()->prefetch.setValue(false);
        pp->refineFormal.mutableValue()->prefetch.setValue(false);
        cfg.live.mutableValue()->vadSilenceMs.setValue(0);
        check("gemini-voice.cleared.conf", render(cfg));
    }

    // The files must load back through Fcitx5's own parser into the same
    // values.
    {
        GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.cleared.conf");
        const auto &pp = cfg.postProcess.value();
        REQUIRE(*pp.refineFixTypos.value().prefetch == false);
        REQUIRE(*pp.refineFormal.value().prefetch == false);
        REQUIRE(*cfg.live.value().vadSilenceMs == 0);
    }
    {
        GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.conf");
        const auto &pp = cfg.postProcess.value();
        REQUIRE(*pp.refineFixTypos.value().prefetch == false);
        REQUIRE(*pp.refineFormal.value().prefetch == true);
        REQUIRE(*pp.refineFormal.value().model == "gemini-4-hydrogen/high");
        REQUIRE(*pp.translate.value().prefetch == true);
        REQUIRE(pp.actions.value().size() == 2);
        REQUIRE(*pp.actions.value()[0].model == "gemini-3.5-flash/low");
        // A name outside the dropdown list is kept verbatim, not reset.
        REQUIRE(*pp.actions.value()[1].name == "translate_pig_latin");
        REQUIRE(*cfg.live.value().mode == fcitx_gemini_voice::LiveMode::SMART);
        REQUIRE(cfg.live.value().customVocabulary.value()[1] == "Pipe Wire");
    }

    // Saving what was loaded must reproduce the file, custom action name
    // included (what Fcitx5 does when the config tool applies the page).
    {
        GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.conf");
        REQUIRE(render(cfg) ==
                slurp(std::string(GOLDEN_DIR) + "/gemini-voice.conf"));
    }

    // The Actions name is described as a string dropdown with i18n labels.
    {
        ActionOverrideConfig a;
        fcitx::RawConfig desc;
        a.dumpDescription(desc);
        auto val = [&](const std::string &path) {
            const auto *v =
                desc.valueByPath("ActionOverrideConfig/Name/" + path);
            return v ? *v : std::string("<missing>");
        };
        REQUIRE(val("Type") == "String");
        REQUIRE(val("IsEnum") == "True");
        REQUIRE(val("Enum/1") == "refine_fix_typos");
        REQUIRE(val("EnumI18n/1") == "Refine: Fix typos");
        REQUIRE(val("Enum/9") == "translate_ja");
        REQUIRE(val("EnumI18n/9") == "Translate: Japanese");
    }

    // The file the Go daemon writes must load through Fcitx5's parser too.
    {
        GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.go-written.conf");
        REQUIRE(*cfg.language == "en-US");
        REQUIRE(*cfg.showActionMenu == false);
        REQUIRE(cfg.translateLanguages.value().size() == 2);
        REQUIRE(cfg.translateLanguages.value()[1] == "Pig Latin");
        REQUIRE(*cfg.live.value().mode == fcitx_gemini_voice::LiveMode::SMART);
        REQUIRE(*cfg.live.value().vadSilenceMs == 0);
        REQUIRE(cfg.live.value().customVocabulary.value()[1] == "Pipe Wire");
        const auto &pp = cfg.postProcess.value();
        REQUIRE(*pp.refineFixTypos.value().prefetch == false);
        REQUIRE(*pp.refineFormal.value().prompt ==
                "Be formal.\nSecond \"line\".");
        REQUIRE(*pp.translate.value().model == "gemini-3.5-flash/low");
        REQUIRE(pp.actions.value().size() == 2);
        REQUIRE(*pp.actions.value()[0].name == "translate_ja");
        REQUIRE(*pp.actions.value()[1].prompt == "Translate to Pig Latin.");
        REQUIRE(*cfg.connection.value().proxy == "http://127.0.0.1:8080");
        REQUIRE(*cfg.connection.value().liveWsUrl == "wss://x.invalid/live");
    }
    return 0;
}
