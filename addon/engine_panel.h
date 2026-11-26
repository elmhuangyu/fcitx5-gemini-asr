#ifndef FCITX5_GEMINI_VOICE_ENGINE_PANEL_H
#define FCITX5_GEMINI_VOICE_ENGINE_PANEL_H

#include <functional>
#include <memory>
#include <string>
#include <utility>

#include <fcitx-utils/key.h>
#include <fcitx-utils/keysym.h>
#include <fcitx/candidatelist.h>
#include <fcitx/inputcontext.h>
#include <fcitx/text.h>

namespace fcitx_gemini_voice {

class ActionCandidateWord : public fcitx::CandidateWord {
public:
    using Callback = std::function<void(fcitx::InputContext *)>;

    ActionCandidateWord(fcitx::Text text, Callback cb,
                        bool emptyCustomLabel = false)
        : fcitx::CandidateWord(std::move(text)), cb_(std::move(cb)) {
        if (emptyCustomLabel) {
            setCustomLabel(fcitx::Text());
        }
    }

    void applyCustomLabel(fcitx::Text label) {
        setCustomLabel(std::move(label));
    }

    void select(fcitx::InputContext *ic) const override {
        if (cb_) {
            cb_(ic);
        }
    }

private:
    Callback cb_;
};

inline bool isHeaderAtPageStart(const fcitx::CandidateList &candList) {
    return candList.size() > 0 && candList.candidate(0).hasCustomLabel() &&
           candList.candidate(0).customLabel().empty();
}

inline fcitx::KeyList defaultSelectionKeys() {
    return {
        fcitx::Key(FcitxKey_1), fcitx::Key(FcitxKey_2), fcitx::Key(FcitxKey_3),
        fcitx::Key(FcitxKey_4), fcitx::Key(FcitxKey_5), fcitx::Key(FcitxKey_6),
        fcitx::Key(FcitxKey_7), fcitx::Key(FcitxKey_8), fcitx::Key(FcitxKey_9),
    };
}

fcitx::Text buildMultilineDisplayText(const std::string &wrappedPreedit);

void applyPanel(fcitx::InputContext *ic, const std::string &auxUp,
                const std::string &wrappedPreedit, bool isMultiline,
                std::unique_ptr<fcitx::CandidateList> candList);

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_ENGINE_PANEL_H
