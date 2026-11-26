#include "engine.h"
#include "engine_panel.h"
#include "engine_targets.h"
#include "engine_text.h"
#include "json_util.h"

#include <utility>

#include <fcitx-utils/i18n.h>
#include <fcitx/candidatelist.h>
#include <fcitx/inputpanel.h>
#include <fcitx/text.h>

namespace fcitx_gemini_voice {

std::vector<TranslateTarget>
GeminiVoiceEngine::effectiveTranslateTargets() const {
    auto configured = resolveConfiguredTargets(*config_.translateLanguages);
    if (!configured.empty()) {
        return configured;
    }
    if (!translateTargets_.empty()) {
        return translateTargets_;
    }
    return {{"translate_en", "English"}};
}

void GeminiVoiceEngine::showActionMenu(fcitx::InputContext *ic) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    state_ = State::ACTION_MENU;
    auto list = std::make_unique<fcitx::CommonCandidateList>();
    list->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
    list->setPageSize(9);
    list->setSelectionKey(defaultSelectionKeys());
    list->setCursorPositionAfterPaging(
        fcitx::CursorPositionAfterPaging::ResetToFirst);

    const std::string preview = truncateUtf8(rawText_, 40);
    list->append<ActionCandidateWord>(
        fcitx::Text(std::string(_("[Raw]")) + " " + preview),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            std::string toCommit = rawText_;
            clearSession(targetIc);
            if (targetIc && !toCommit.empty()) {
                targetIc->commitString(toCommit);
            }
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(
            _("[Refine ▸] Polish (Fix typos / Casual / Formal / As Prompt)")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            showRefineSubmenu(targetIc);
        });
    const auto targets = effectiveTranslateTargets();
    if (targets.size() == 1) {
        const auto &target = targets[0];
        const std::string translateLabel =
            (target.action == "translate_en")
                ? std::string(_("[Translate] Translate to English"))
                : (std::string(_("[Translate] Translate to ")) + target.label);
        list->append<ActionCandidateWord>(
            fcitx::Text(translateLabel),
            [this, alive = alive_, action = target.action,
             label = target.label](fcitx::InputContext *targetIc) {
                if (!alive->load()) {
                    return;
                }
                triggerPostProcess(targetIc, action, label);
            });
    } else {
        list->append<ActionCandidateWord>(
            fcitx::Text(_("[Translate ▸] Translate to ...")),
            [this, alive = alive_](fcitx::InputContext *targetIc) {
                if (!alive->load()) {
                    return;
                }
                showTranslateSubmenu(targetIc);
            });
    }
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[Ask AI] Use speech as prompt to generate response")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "ask_ai");
        });

    applyMenuPanel(ic, "", rawText_, std::move(list));
}

void GeminiVoiceEngine::showRefineSubmenu(fcitx::InputContext *ic) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    state_ = State::SUBMENU_REFINE;
    auto list = std::make_unique<fcitx::CommonCandidateList>();
    list->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
    list->setPageSize(9);
    list->setSelectionKey(defaultSelectionKeys());
    list->setCursorPositionAfterPaging(
        fcitx::CursorPositionAfterPaging::ResetToFirst);

    list->append<ActionCandidateWord>(
        fcitx::Text(_(
            "[Fix Typos] Fix typos and punctuation only, keeping spoken tone")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_fix_typos");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[Casual] Natural and relaxed tone for chat")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_casual");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(
            _("[Formal] Clear and professional tone for email and docs")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "refine_formal");
        });
    list->append<ActionCandidateWord>(
        fcitx::Text(_("[As Prompt] Convert to structured prompt")),
        [this, alive = alive_](fcitx::InputContext *targetIc) {
            if (!alive->load()) {
                return;
            }
            triggerPostProcess(targetIc, "as_prompt");
        });

    applyMenuPanel(ic, _("Refine ▸"), rawText_, std::move(list));
}

void GeminiVoiceEngine::showTranslateSubmenu(fcitx::InputContext *ic) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    state_ = State::SUBMENU_TRANSLATE;
    auto list = std::make_unique<fcitx::CommonCandidateList>();
    list->setLayoutHint(fcitx::CandidateLayoutHint::Vertical);
    list->setPageSize(9);
    list->setSelectionKey(defaultSelectionKeys());
    list->setCursorPositionAfterPaging(
        fcitx::CursorPositionAfterPaging::ResetToFirst);

    for (const auto &target : effectiveTranslateTargets()) {
        list->append<ActionCandidateWord>(
            fcitx::Text(target.label),
            [this, alive = alive_, action = target.action,
             label = target.label](fcitx::InputContext *targetIc) {
                if (!alive->load()) {
                    return;
                }
                triggerPostProcess(targetIc, action, label);
            });
    }

    applyMenuPanel(ic, _("Translate ▸"), rawText_, std::move(list));
}

void GeminiVoiceEngine::triggerPostProcess(fcitx::InputContext *ic,
                                           const std::string &action,
                                           const std::string &targetLabel) {
    if (!ic) {
        clearSession(nullptr);
        return;
    }

    std::string cmd =
        targetLabel.empty()
            ? buildCommandJson("post_process", {{"action", action}})
            : buildCommandJson("post_process", {{"action", action},
                                                {"target_label", targetLabel}});
    if (!socketClient_->sendLine(cmd)) {
        instance_->showCustomInputMethodInformation(
            ic, _("Gemini Voice Daemon not connected"));
        showActionMenu(ic);
        return;
    }

    state_ = State::PROCESSING;
    ic->inputPanel().setAuxDown(fcitx::Text());
    ic->inputPanel().setCandidateList(nullptr);
    setPanelDisplay(ic, _("[AI Processing...]"), "");
}

} // namespace fcitx_gemini_voice
