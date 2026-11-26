#include "engine.h"
#include "engine_panel.h"
#include "engine_text.h"

#include <fcitx-utils/keysym.h>
#include <fcitx/inputpanel.h>

namespace fcitx_gemini_voice {

fcitx::Text buildMultilineDisplayText(const std::string &wrappedPreedit) {
    fcitx::Text out;
    if (!wrappedPreedit.empty()) {
        out.append(wrappedPreedit, fcitx::TextFormatFlag::Underline);
    }
    return out;
}

void applyPanel(fcitx::InputContext *ic, const std::string &auxUp,
                const std::string &wrappedPreedit, bool isMultiline,
                std::unique_ptr<fcitx::CandidateList> candList) {
    auto &panel = ic->inputPanel();
    panel.setClientPreedit(fcitx::Text());
    panel.setAuxDown(fcitx::Text());

    if (isMultiline) {
        // Keep preedit non-empty in upperLayout_ while placing all wrapped
        // text lines inside a single CandidateWord MultilineLayout so line
        // spacing across lines 1..N is uniform.
        panel.setAuxUp(fcitx::Text());
        panel.setPreedit(auxUp.empty() ? fcitx::Text() : fcitx::Text(auxUp));
    } else {
        fcitx::Text formattedPreedit;
        if (!wrappedPreedit.empty()) {
            formattedPreedit.append(wrappedPreedit,
                                    fcitx::TextFormatFlag::Underline);
            formattedPreedit.setCursor(static_cast<int>(wrappedPreedit.size()));
        }

        std::string displayAuxUp = auxUp;
        if (!displayAuxUp.empty() && !wrappedPreedit.empty() &&
            displayAuxUp.back() != ' ' && displayAuxUp.back() != '\t') {
            displayAuxUp.push_back(' ');
        }

        panel.setAuxUp(displayAuxUp.empty() ? fcitx::Text()
                                            : fcitx::Text(displayAuxUp));
        panel.setPreedit(formattedPreedit);
    }

    panel.setCandidateList(std::move(candList));
    ic->updatePreedit();
    ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
}

void GeminiVoiceEngine::setPanelDisplay(fcitx::InputContext *ic,
                                        const std::string &auxUp,
                                        const std::string &preeditText) {
    if (!ic) {
        return;
    }
    const std::string wrapped =
        wrapPreeditText(preeditText, *config_.preeditMaxWidth);
    const bool isMultiline = wrapped.find('\n') != std::string::npos;

    // Fcitx5 classicui forces upperLayout_ (auxUp + preedit) into Pango
    // single-paragraph mode, and hardcodes auxDownTextNode_'s Yoga height to
    // a single fontHeight (so >=2 lines in auxDown overflow and clip at ~2.5
    // lines). When wrapped text spans multiple lines, place all lines in a
    // vertical CandidateList item so height and line spacing are uniform.
    std::unique_ptr<fcitx::CommonCandidateList> contList;
    if (isMultiline) {
        contList = std::make_unique<fcitx::CommonCandidateList>();
        contList->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
        contList->setPageSize(1);
        contList->setLabels({""});
        contList->append<ActionCandidateWord>(
            buildMultilineDisplayText(wrapped), ActionCandidateWord::Callback{},
            true);
        contList->setGlobalCursorIndex(-1);
    }

    applyPanel(ic, auxUp, wrapped, isMultiline, std::move(contList));
}

void GeminiVoiceEngine::applyMenuPanel(
    fcitx::InputContext *ic, const std::string &auxUp,
    const std::string &rawText,
    std::unique_ptr<fcitx::CommonCandidateList> list) {
    if (!ic || !list) {
        return;
    }
    const std::string wrapped =
        wrapPreeditText(rawText, *config_.preeditMaxWidth);
    const bool isMultiline = wrapped.find('\n') != std::string::npos;

    std::string effectiveAuxUp = auxUp;
    if (isMultiline) {
        if (effectiveAuxUp.empty()) {
            effectiveAuxUp = *config_.recordingPrefix;
        }
        const int actionCount = list->totalSize();
        list->insert(0, std::make_unique<ActionCandidateWord>(
                            buildMultilineDisplayText(wrapped),
                            ActionCandidateWord::Callback{}, true));
        const int pageSize = list->pageSize();
        for (int i = 1; i <= actionCount; ++i) {
            const int labelNum =
                (pageSize > 0 && i >= pageSize) ? ((i % pageSize) + 1) : i;
            if (auto *word = dynamic_cast<ActionCandidateWord *>(
                    const_cast<fcitx::CandidateWord *>(
                        &list->candidateFromAll(i)))) {
                word->applyCustomLabel(
                    fcitx::Text(std::to_string(labelNum) + ". "));
            }
        }
        list->setGlobalCursorIndex(1);
    } else {
        list->setGlobalCursorIndex(0);
    }

    applyPanel(ic, effectiveAuxUp, wrapped, isMultiline, std::move(list));
}

bool GeminiVoiceEngine::handleCandidateMenuKey(fcitx::InputContext *ic,
                                               fcitx::CandidateList &candList,
                                               const fcitx::Key &key) {
    const int minIdx = isHeaderAtPageStart(candList) ? 1 : 0;

    int digit = key.digitSelection();
    if (digit >= 0) {
        int target = digit + minIdx;
        if (target >= minIdx && target < candList.size()) {
            candList.candidate(target).select(ic);
        }
        return true;
    }

    if (key.check(FcitxKey_space) || key.check(FcitxKey_Return) ||
        key.check(FcitxKey_KP_Enter)) {
        int idx = candList.cursorIndex();
        if (idx >= minIdx && idx < candList.size()) {
            candList.candidate(idx).select(ic);
        }
        return true;
    }

    if (key.check(FcitxKey_Up)) {
        if (auto *movable = candList.toCursorMovable()) {
            movable->prevCandidate();
            if (isHeaderAtPageStart(candList) && candList.cursorIndex() == 0) {
                movable->prevCandidate();
            }
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return true;
    }

    if (key.check(FcitxKey_Down)) {
        if (auto *movable = candList.toCursorMovable()) {
            movable->nextCandidate();
            if (isHeaderAtPageStart(candList) && candList.cursorIndex() == 0) {
                movable->nextCandidate();
            }
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return true;
    }

    if (key.check(FcitxKey_Page_Down)) {
        if (auto *pageable = candList.toPageable();
            pageable && pageable->hasNext()) {
            pageable->next();
            if (isHeaderAtPageStart(candList) && candList.cursorIndex() == 0) {
                if (auto *movable = candList.toCursorMovable()) {
                    movable->nextCandidate();
                }
            }
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return true;
    }

    if (key.check(FcitxKey_Page_Up)) {
        if (auto *pageable = candList.toPageable();
            pageable && pageable->hasPrev()) {
            pageable->prev();
            if (isHeaderAtPageStart(candList) && candList.cursorIndex() == 0) {
                if (auto *movable = candList.toCursorMovable()) {
                    movable->nextCandidate();
                }
            }
            ic->updateUserInterface(fcitx::UserInterfaceComponent::InputPanel);
        }
        return true;
    }

    return false;
}

} // namespace fcitx_gemini_voice
