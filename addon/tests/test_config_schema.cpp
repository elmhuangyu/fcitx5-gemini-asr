// Verifies the on-disk serialization of GeminiVoiceConfig, the single source of
// truth shared with the Go daemon. The daemon's loader tests parse the golden
// files this test checks, so both sides agree on real Fcitx5 INI output.
//
// Rerun with UPDATE_GOLDEN=1 to regenerate the golden files.

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

} // namespace

int main() {
    ::setenv("LC_ALL", "C", 1);
    ::setenv("LANGUAGE", "C", 1);

    // All defaults: Fcitx5 writes default-valued options as commented lines.
    {
        fcitx_gemini_voice::GeminiVoiceConfig cfg;
        check("gemini-voice.defaults.conf", render(cfg));
    }

    // Edge case: lists explicitly cleared and an empty-string override.
    {
        fcitx_gemini_voice::GeminiVoiceConfig cfg;
        cfg.postProcess.mutableValue()->prefetch.setValue(
            std::vector<std::string>{});
        cfg.live.mutableValue()->vadSilenceMs.setValue(0);
        check("gemini-voice.cleared.conf", render(cfg));
    }

    // The files must also load back through Fcitx5's own parser into the
    // same values (an empty Prefetch= means "no prefetch", not the default).
    {
        fcitx_gemini_voice::GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.cleared.conf");
        REQUIRE(cfg.postProcess.value().prefetch.value().empty());
        REQUIRE(*cfg.live.value().vadSilenceMs == 0);
    }
    {
        fcitx_gemini_voice::GeminiVoiceConfig cfg;
        fcitx::readAsIni(cfg, fcitx::StandardPathsType::Config,
                         std::filesystem::path(GOLDEN_DIR) /
                             "gemini-voice.conf");
        REQUIRE(cfg.postProcess.value().prefetch.value().size() == 2);
        REQUIRE(cfg.postProcess.value().actions.value().size() == 2);
        REQUIRE(*cfg.postProcess.value().actions.value()[1].model ==
                "gemini-3.5-flash");
        REQUIRE(*cfg.live.value().mode == fcitx_gemini_voice::LiveMode::SMART);
        REQUIRE(cfg.live.value().customVocabulary.value()[1] == "Pipe Wire");
    }
    return 0;
}
