#include "json_util.h"

#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <functional>
#include <iostream>
#include <memory>
#include <mutex>
#include <poll.h>
#include <sstream>
#include <string>
#include <sys/socket.h>
#include <sys/un.h>
#include <thread>
#include <unistd.h>
#include <vector>

#include <fcitx-config/rawconfig.h>
#include <fcitx-utils/capabilityflags.h>
#include <fcitx-utils/event.h>
#include <fcitx-utils/eventdispatcher.h>
#include <fcitx-utils/key.h>
#include <fcitx-utils/keysym.h>
#include <fcitx/addonmanager.h>
#include <fcitx/candidatelist.h>
#include <fcitx/event.h>
#include <fcitx/inputcontext.h>
#include <fcitx/inputmethodmanager.h>
#include <fcitx/inputpanel.h>
#include <fcitx/instance.h>

#define REQUIRE(cond)                                                          \
    do {                                                                       \
        if (!(cond)) {                                                         \
            std::cerr << "FAIL at " << __FILE__ << ":" << __LINE__ << ": "     \
                      << #cond << "\n";                                        \
            std::abort();                                                      \
        }                                                                      \
    } while (0)

namespace fs = std::filesystem;

namespace {

class TestInputContext : public fcitx::InputContext {
public:
    explicit TestInputContext(fcitx::InputContextManager &manager)
        : fcitx::InputContext(manager, "test-app") {
        created();
    }
    ~TestInputContext() override { destroy(); }

    const char *frontend() const override { return "test"; }

    std::vector<std::string> committed;
    std::string lastClientPreedit;
    int updatePreeditCount = 0;

protected:
    void commitStringImpl(const std::string &text) override {
        committed.push_back(text);
    }
    void deleteSurroundingTextImpl(int /*offset*/,
                                   unsigned int /*size*/) override {}
    void forwardKeyImpl(const fcitx::ForwardKeyEvent & /*key*/) override {}
    void updatePreeditImpl() override {
        ++updatePreeditCount;
        lastClientPreedit = inputPanel().clientPreedit().toString();
    }
};

class MockDaemonServer {
public:
    explicit MockDaemonServer(std::string sockPath)
        : sockPath_(std::move(sockPath)) {
        int prc = ::pipe2(wakeFds_, O_CLOEXEC | O_NONBLOCK);
        REQUIRE(prc == 0);
        listenFd_ = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
        REQUIRE(listenFd_ >= 0);

        sockaddr_un addr{};
        REQUIRE(sockPath_.size() < sizeof(addr.sun_path));
        addr.sun_family = AF_UNIX;
        std::memcpy(addr.sun_path, sockPath_.c_str(), sockPath_.size() + 1);
        ::unlink(sockPath_.c_str());
        int rc = ::bind(listenFd_, reinterpret_cast<sockaddr *>(&addr),
                        sizeof(addr));
        REQUIRE(rc == 0);
        rc = ::listen(listenFd_, 4);
        REQUIRE(rc == 0);

        thread_ = std::thread(&MockDaemonServer::run, this);
    }

    ~MockDaemonServer() { stop(); }

    void stop() {
        if (stopped_.exchange(true)) {
            return;
        }
        if (wakeFds_[1] >= 0) {
            char b = 1;
            ssize_t ignored = ::write(wakeFds_[1], &b, 1);
            (void)ignored;
        }
        if (thread_.joinable()) {
            thread_.join();
        }
        if (clientFd_ >= 0) {
            ::close(clientFd_);
            clientFd_ = -1;
        }
        if (listenFd_ >= 0) {
            ::close(listenFd_);
            listenFd_ = -1;
        }
        if (wakeFds_[0] >= 0) {
            ::close(wakeFds_[0]);
            wakeFds_[0] = -1;
        }
        if (wakeFds_[1] >= 0) {
            ::close(wakeFds_[1]);
            wakeFds_[1] = -1;
        }
        ::unlink(sockPath_.c_str());
    }

    std::string waitForCommand(int index, int timeoutMs = 2000) {
        std::unique_lock<std::mutex> lock(mu_);
        bool ok =
            cv_.wait_for(lock, std::chrono::milliseconds(timeoutMs), [&]() {
                return static_cast<int>(commands_.size()) > index;
            });
        REQUIRE(ok);
        return commands_[static_cast<size_t>(index)];
    }

    void sendEvent(const std::string &jsonLine) {
        std::unique_lock<std::mutex> lock(mu_);
        bool connected = cv_.wait_for(lock, std::chrono::milliseconds(2000),
                                      [&]() { return clientFd_ >= 0; });
        REQUIRE(connected);
        int fd = clientFd_;
        std::string payload = jsonLine + "\n";
        ssize_t n = ::send(fd, payload.data(), payload.size(), MSG_NOSIGNAL);
        REQUIRE(n == static_cast<ssize_t>(payload.size()));
    }

private:
    void run() {
        std::string buf;
        while (!stopped_.load()) {
            pollfd pfds[3]{};
            nfds_t nfds = 0;
            pfds[nfds++] = {wakeFds_[0], POLLIN, 0};
            pfds[nfds++] = {listenFd_, POLLIN, 0};
            if (clientFd_ >= 0) {
                pfds[nfds++] = {clientFd_, POLLIN, 0};
            }

            int rc = ::poll(pfds, nfds, 100);
            if (rc <= 0) {
                continue;
            }
            if (pfds[0].revents & POLLIN) {
                char tmp[32];
                while (::read(wakeFds_[0], tmp, sizeof(tmp)) > 0) {
                }
            }
            if (stopped_.load()) {
                break;
            }

            if (pfds[1].revents & POLLIN) {
                int cfd = ::accept4(listenFd_, nullptr, nullptr, SOCK_CLOEXEC);
                if (cfd >= 0) {
                    std::lock_guard<std::mutex> lock(mu_);
                    if (clientFd_ >= 0) {
                        ::close(clientFd_);
                    }
                    clientFd_ = cfd;
                    buf.clear();
                    cv_.notify_all();
                }
            } else if (nfds == 3 &&
                       (pfds[2].revents & (POLLIN | POLLHUP | POLLERR))) {
                char tmp[1024];
                ssize_t n = ::recv(clientFd_, tmp, sizeof(tmp), MSG_DONTWAIT);
                if (n > 0) {
                    buf.append(tmp, static_cast<size_t>(n));
                    size_t start = 0;
                    while (true) {
                        size_t nl = buf.find('\n', start);
                        if (nl == std::string::npos) {
                            break;
                        }
                        std::string line = buf.substr(start, nl - start);
                        if (!line.empty()) {
                            std::lock_guard<std::mutex> lock(mu_);
                            commands_.push_back(std::move(line));
                            cv_.notify_all();
                        }
                        start = nl + 1;
                    }
                    if (start > 0) {
                        buf.erase(0, start);
                    }
                } else if (n == 0 ||
                           (n < 0 && errno != EAGAIN && errno != EWOULDBLOCK)) {
                    std::lock_guard<std::mutex> lock(mu_);
                    ::close(clientFd_);
                    clientFd_ = -1;
                    buf.clear();
                }
            }
        }
    }

    std::string sockPath_;
    int listenFd_ = -1;
    int clientFd_ = -1;
    int wakeFds_[2] = {-1, -1};
    std::atomic<bool> stopped_{false};
    std::thread thread_;

    std::mutex mu_;
    std::condition_variable cv_;
    std::vector<std::string> commands_;
};

void runOnMain(fcitx::Instance &instance, const std::function<void()> &fn) {
    std::mutex mu;
    std::condition_variable cv;
    bool done = false;
    instance.eventDispatcher().schedule([&]() {
        fn();
        {
            std::lock_guard<std::mutex> lock(mu);
            done = true;
        }
        cv.notify_one();
    });
    std::unique_lock<std::mutex> lock(mu);
    cv.wait(lock, [&]() { return done; });
}

bool waitOnMain(fcitx::Instance &instance, const std::function<bool()> &pred,
                int timeoutMs = 2000) {
    auto deadline =
        std::chrono::steady_clock::now() + std::chrono::milliseconds(timeoutMs);
    while (std::chrono::steady_clock::now() < deadline) {
        bool ok = false;
        runOnMain(instance, [&]() { ok = pred(); });
        if (ok) {
            return true;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    return false;
}

void sendKey(fcitx::InputContext *ic, const fcitx::Key &key,
             bool expectFiltered = true) {
    fcitx::KeyEvent ev(ic, key, false);
    ic->keyEvent(ev);
    REQUIRE(ev.filtered() == expectFiltered);
}

} // namespace

int main() {
    ::setenv("LC_ALL", "C", 1);
    ::setenv("LANGUAGE", "C", 1);

    char tmpl[] = "/tmp/fcitx5-gemini-test-XXXXXX";
    char *tmpDirPtr = ::mkdtemp(tmpl);
    REQUIRE(tmpDirPtr != nullptr);
    fs::path root(tmpDirPtr);

    fs::path addonDir = root / "addons";
    fs::path dataDir = root / "share";
    fs::path configDir = root / "config";
    fs::path dataHomeDir = root / "data_home";
    fs::path runtimeDir = root / "runtime";
    fs::create_directories(addonDir);
    fs::create_directories(dataDir / "fcitx5" / "addon");
    fs::create_directories(configDir);
    fs::create_directories(dataHomeDir);
    fs::create_directories(runtimeDir);

    fs::copy_file(BUILD_ADDON_SO, addonDir / "libgemini-voice.so",
                  fs::copy_options::overwrite_existing);
    fs::copy_file(BUILD_ADDON_CONF,
                  dataDir / "fcitx5" / "addon" / "gemini-voice.conf",
                  fs::copy_options::overwrite_existing);

    std::string dataDirs = dataDir.string() + ":/usr/share";
    ::setenv("FCITX_ADDON_DIRS", addonDir.c_str(), 1);
    ::setenv("XDG_DATA_DIRS", dataDirs.c_str(), 1);
    ::setenv("XDG_CONFIG_HOME", configDir.c_str(), 1);
    ::setenv("XDG_DATA_HOME", dataHomeDir.c_str(), 1);
    ::setenv("XDG_RUNTIME_DIR", runtimeDir.c_str(), 1);

    std::string sockPath = (runtimeDir / "gemini-ime.sock").string();
    MockDaemonServer daemon(sockPath);

    char arg0[] = "test_engine_integration";
    char arg1[] = "--disable=all";
    char arg2[] = "--enable=keyboard,gemini-voice";
    char *argv[] = {arg0, arg1, arg2};

    std::shared_ptr<fcitx::CandidateList> staleList;
    {
        fcitx::Instance instance(3, argv);
        instance.addonManager().registerDefaultLoader(nullptr);
        instance.initialize();
        instance.setRunning(true);

        std::thread driver([&]() {
            fcitx::AddonInstance *addon = nullptr;
            std::unique_ptr<TestInputContext> ic;

            runOnMain(instance, [&]() {
                addon = instance.addonManager().addon("gemini-voice", true);
                REQUIRE(addon != nullptr);
                // Loaded as Module, not InputMethod.
                REQUIRE(instance.inputMethodManager().entry("gemini-voice") ==
                        nullptr);

                ic = std::make_unique<TestInputContext>(
                    instance.inputContextManager());
                ic->setCapabilityFlags(fcitx::CapabilityFlag::Preedit);
                ic->focusIn();
            });

            int cmdIdx = 0;

            // 0. Normal key while IDLE passes through (not filtered).
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_a), false);
            });

            // 1. TriggerKey (Alt+space) -> start_recording + floating
            // InputPanel (clientPreedit stays empty even with
            // CapabilityFlag::Preedit).
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "start_recording");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().clientPreedit().empty());
                    REQUIRE(ic->inputPanel().auxUp().toString() == "🎙️");
                    REQUIRE(ic->inputPanel().preedit().empty());
                });
            }

            // 2. partial_text updates floating InputPanel preedit while
            // keeping clientPreedit empty.
            daemon.sendEvent(R"({"event":"partial_text","text":"今天下午"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().auxUp().toString() == "🎙️ " &&
                       ic->inputPanel().preedit().toString() == "今天下午" &&
                       ic->inputPanel().clientPreedit().empty();
            }));

            // 3. Return -> stop_recording -> transcribe_done -> ACTION_MENU ->
            // Space commits raw_text
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "stop_recording");
            }
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"今天下午三点我们开个会"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4 &&
                       cl->layoutHint() ==
                           fcitx::CandidateLayoutHint::Vertical &&
                       ic->inputPanel().preedit().toString() ==
                           "今天下午三点我们开个会" &&
                       ic->inputPanel().clientPreedit().empty();
            }));
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_space));
                REQUIRE(ic->committed.size() == 1);
                REQUIRE(ic->committed[0] == "今天下午三点我们开个会");
                REQUIRE(!ic->inputPanel().candidateList());
                REQUIRE(ic->inputPanel().clientPreedit().empty());
                REQUIRE(ic->inputPanel().preedit().empty());
            });

            // 4. Floating InputPanel (with CapabilityFlag::Preedit unset) +
            // Level 2 Refine navigation
            runOnMain(instance, [&]() {
                ic->setCapabilityFlags(fcitx::CapabilityFlags{});
                sendKey(ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);

            daemon.sendEvent(
                R"({"event":"partial_text","text":"测试降级预览"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().preedit().toString() ==
                           "测试降级预览" &&
                       ic->inputPanel().clientPreedit().empty();
            }));

            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"把这段话改得正式一点"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));

            // Right -> Level 2 Refine submenu (4 options), Left -> back to
            // Level 1, Right -> '3' -> refine_formal
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Right));
                auto cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 4);
                REQUIRE(ic->inputPanel().auxUp().toString() == "Refine ▸ ");
                REQUIRE(ic->inputPanel().clientPreedit().empty());

                sendKey(ic.get(), fcitx::Key(FcitxKey_Left));
                cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 4);

                sendKey(ic.get(), fcitx::Key(FcitxKey_Right));
                sendKey(ic.get(), fcitx::Key(FcitxKey_3));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "refine_formal");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().auxUp().toString() ==
                            "[AI Processing...]");
                    REQUIRE(ic->inputPanel().clientPreedit().empty());
                });
            }

            // 5. Error during PROCESSING falls back to ACTION_MENU preserving
            // raw_text, then translate_en succeeds
            daemon.sendEvent(
                R"({"event":"error","message":"temporary glitch"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key(FcitxKey_3)); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "translate_en");
            }
            daemon.sendEvent(
                R"({"event":"final_result","text":"Please make this paragraph more formal."})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 2; }));
            runOnMain(instance, [&]() {
                REQUIRE(ic->committed[1] ==
                        "Please make this paragraph more formal.");
            });

            // 5b. Digit '4' sends post_process("ask_ai")
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"帮我写个脚本"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key(FcitxKey_4)); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "ask_ai");
            }
            daemon.sendEvent(
                R"({"event":"final_result","text":"#!/bin/bash\necho hello"})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 3; }));
            runOnMain(instance, [&]() {
                REQUIRE(ic->committed[2] == "#!/bin/bash\necho hello");
            });

            // 5c. Refine submenu item '4' sends post_process("as_prompt")
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"帮我写个脚本"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key(FcitxKey_2)); });
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4 &&
                       ic->inputPanel().auxUp().toString() == "Refine ▸ ";
            }));
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key(FcitxKey_4)); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "as_prompt");
            }
            daemon.sendEvent(
                R"({"event":"final_result","text":"Prompt: Write a script..."})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 4; }));
            runOnMain(instance, [&]() {
                REQUIRE(ic->committed[3] == "Prompt: Write a script...");
            });

            // 5d. Single non-English translate target selects directly
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"你好","translate_targets":[{"action":"translate_ja","label":"Japanese"}]})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key(FcitxKey_3)); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "translate_ja");
            }
            daemon.sendEvent(R"({"event":"final_result","text":"こんにちは"})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 5; }));

            // 5e. Multiple translate targets open Level 2 Translate submenu
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"你好世界","translate_targets":[{"action":"translate_en","label":"English"},{"action":"translate_ja","label":"Japanese"},{"action":"translate_fr","label":"French"}]})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            // Navigate down to [Translate ▸] and press Right -> Level 2 submenu
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Down));
                sendKey(ic.get(), fcitx::Key(FcitxKey_Down));
                sendKey(ic.get(), fcitx::Key(FcitxKey_Right));
                auto cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 3);
                REQUIRE(ic->inputPanel().auxUp().toString() == "Translate ▸ ");

                // Left returns to Level 1, '3' also opens Translate submenu
                sendKey(ic.get(), fcitx::Key(FcitxKey_Left));
                cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 4);
                sendKey(ic.get(), fcitx::Key(FcitxKey_3));
                cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 3);
                sendKey(ic.get(), fcitx::Key(FcitxKey_2));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "translate_ja");
            }
            daemon.sendEvent(
                R"({"event":"final_result","text":"こんにちは世界"})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 6; }));

            // 6. Escape during RECORDING sends cancel and clears UI
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Escape));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "cancel");
                runOnMain(instance,
                          [&]() { REQUIRE(ic->inputPanel().empty()); });
            }

            // 6a. FocusOut during RECORDING cancels active session
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() { ic->focusOut(); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "cancel");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().empty());
                    ic->focusIn();
                });
            }

            // 6b. Daemon `state_changed`: echoes of engine-initiated
            // transitions are ignored; an unexpected daemon-side reset
            // (idle with no result) recovers to idle with feedback.
            runOnMain(instance, [&]() {
                ic->setCapabilityFlags(fcitx::CapabilityFlag::Preedit);
                sendKey(ic.get(), fcitx::Key("Alt+space"));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "start_recording");
            }
            // Echo of the engine-initiated start must not reset anything:
            // the engine is still RECORDING and accepts partial_text.
            daemon.sendEvent(
                R"({"event":"state_changed","state":"recording"})");
            daemon.sendEvent(R"({"event":"partial_text","text":"仍在录音中"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().preedit().toString() == "仍在录音中" &&
                       ic->inputPanel().clientPreedit().empty();
            }));

            // Unexpected daemon-side reset: idle arrives with no result
            // while RECORDING -> back to idle, panel cleared, status shown,
            // nothing committed.
            daemon.sendEvent(R"({"event":"state_changed","state":"idle"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return !ic->inputPanel().candidateList() &&
                       ic->inputPanel().clientPreedit().empty() &&
                       ic->inputPanel().preedit().empty() &&
                       ic->inputPanel().overlayMessage().toString() ==
                           "Voice session ended unexpectedly";
            }));
            runOnMain(instance, [&]() { REQUIRE(ic->committed.size() == 6); });

            // A subsequent session works normally.
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "start_recording");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().auxUp().toString() == "🎙️");
                    REQUIRE(ic->inputPanel().clientPreedit().empty());
                });
            }
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Escape));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "cancel");
            }
            // The real daemon emits idle after handling our own cancel;
            // that echo must be ignored (engine already idle, no feedback,
            // panel stays clear).
            daemon.sendEvent(R"({"event":"state_changed","state":"idle"})");
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
            runOnMain(instance, [&]() { REQUIRE(ic->inputPanel().empty()); });

            // Same recovery while STOPPING: idle before any
            // transcribe_done clears the finalizing state.
            runOnMain(instance,
                      [&]() { sendKey(ic.get(), fcitx::Key("Alt+space")); });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(R"({"event":"state_changed","state":"idle"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().clientPreedit().empty() &&
                       ic->inputPanel().auxUp().empty() &&
                       ic->inputPanel().preedit().empty() &&
                       !ic->inputPanel().candidateList();
            }));

            // 7. ShowActionMenu=false commits raw_text directly
            runOnMain(instance, [&]() {
                fcitx::RawConfig rawCfg;
                rawCfg.setValueByPath("ShowActionMenu", "False");
                rawCfg.setValueByPath("RecordingPrefix", "🔴");
                rawCfg.setValueByPath("PreeditMaxWidth", "10");
                addon->setConfig(rawCfg);
                sendKey(ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"partial_text","text":"一二三四五六七八九十十一"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().candidateList() != nullptr;
            }));
            runOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                REQUIRE(ic->inputPanel().auxUp().empty());
                REQUIRE(ic->inputPanel().preedit().toString() == "🔴");
                REQUIRE(ic->inputPanel().auxDown().empty());
                REQUIRE(cl != nullptr);
                REQUIRE(cl->size() == 1);
                REQUIRE(cl->candidate(0).hasCustomLabel());
                REQUIRE(cl->candidate(0).customLabel().empty());
                REQUIRE(cl->cursorIndex() == -1);
                REQUIRE(cl->candidate(0).text().toString() ==
                        "一二三四五\n六七八九十\n十一");
            });
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"直接上屏"})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 7; }));
            runOnMain(instance,
                      [&]() { REQUIRE(ic->committed[6] == "直接上屏"); });

            // 7b. Multiline (>3 lines) raw_text in ACTION_MENU & SUBMENU_REFINE
            // places continuation lines in candidate[0] without shifting
            // selection keys or cursor navigation.
            runOnMain(instance, [&]() {
                fcitx::RawConfig rawCfg;
                rawCfg.setValueByPath("ShowActionMenu", "True");
                rawCfg.setValueByPath("PreeditMaxWidth", "10");
                addon->setConfig(rawCfg);
                sendKey(ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"一二三四五六七八九十十一十二十三十四十五"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 5 &&
                       ic->inputPanel().auxUp().empty() &&
                       ic->inputPanel().preedit().toString() == "🔴" &&
                       ic->inputPanel().auxDown().empty() &&
                       cl->candidate(0).hasCustomLabel() &&
                       cl->candidate(0).customLabel().empty() &&
                       cl->candidate(1).hasCustomLabel() &&
                       cl->candidate(1).customLabel().toString() == "1. " &&
                       cl->candidate(0).text().toString() ==
                           "一二三四五\n六七八九十\n十一十二十\n三十四十五" &&
                       cl->cursorIndex() == 1;
            }));
            runOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                // Up from index 1 wraps to last action (index 4), skipping 0.
                sendKey(ic.get(), fcitx::Key(FcitxKey_Up));
                REQUIRE(cl->cursorIndex() == 4);
                // Down from index 4 wraps to index 1, skipping 0.
                sendKey(ic.get(), fcitx::Key(FcitxKey_Down));
                REQUIRE(cl->cursorIndex() == 1);
                // Digit '2' opens Refine submenu (also multiline).
                sendKey(ic.get(), fcitx::Key(FcitxKey_2));
                cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 5);
                REQUIRE(cl->candidate(0).hasCustomLabel());
                REQUIRE(cl->candidate(0).customLabel().empty());
                REQUIRE(cl->cursorIndex() == 1);
                // Back to Action menu and press '1' to commit raw_text.
                sendKey(ic.get(), fcitx::Key(FcitxKey_Left));
                sendKey(ic.get(), fcitx::Key(FcitxKey_1));
                REQUIRE(ic->committed.size() == 8);
                REQUIRE(ic->committed[7] ==
                        "一二三四五六七八九十十一十二十三十四十五");
            });

            // 7c. Paginated Translate submenu (>= 9 targets, multiline header)
            {
                const std::string twelveTargetsJson =
                    R"({"event":"transcribe_done","raw_text":"一二三四五六七八九十十一十二十三十四十五","translate_targets":[)"
                    R"({"action":"translate_en","label":"English"},)"
                    R"({"action":"translate_zh","label":"Chinese"},)"
                    R"({"action":"translate_ja","label":"Japanese"},)"
                    R"({"action":"translate_ko","label":"Korean"},)"
                    R"({"action":"translate_fr","label":"French"},)"
                    R"({"action":"translate_de","label":"German"},)"
                    R"({"action":"translate_es","label":"Spanish"},)"
                    R"({"action":"translate_pt","label":"Portuguese"},)"
                    R"({"action":"translate_ru","label":"Russian"},)"
                    R"({"action":"translate_it","label":"Italian"},)"
                    R"({"action":"translate_ar","label":"Arabic"},)"
                    R"({"action":"translate_vi","label":"Vietnamese"}]})";

                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key("Alt+space"));
                });
                daemon.waitForCommand(cmdIdx++);
                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
                });
                daemon.waitForCommand(cmdIdx++);
                daemon.sendEvent(twelveTargetsJson);
                REQUIRE(waitOnMain(instance, [&]() {
                    auto cl = ic->inputPanel().candidateList();
                    return cl && cl->size() == 5;
                }));
                runOnMain(instance, [&]() {
                    // Open Translate submenu ('3')
                    sendKey(ic.get(), fcitx::Key(FcitxKey_3));
                    auto cl = ic->inputPanel().candidateList();
                    REQUIRE(cl != nullptr);
                    auto *pageable = cl->toPageable();
                    REQUIRE(pageable != nullptr);
                    REQUIRE(pageable->totalPages() == 2);
                    REQUIRE(pageable->currentPage() == 0);
                    REQUIRE(cl->size() == 9);
                    REQUIRE(cl->cursorIndex() == 1);
                    REQUIRE(cl->candidate(0).hasCustomLabel());
                    REQUIRE(cl->candidate(0).customLabel().empty());
                    REQUIRE(cl->candidate(1).customLabel().toString() == "1. ");
                    REQUIRE(cl->candidate(8).customLabel().toString() == "8. ");

                    // Navigate Down across page boundary: must land on item 9
                    // (Russian, page 1 index 0) without skipping it.
                    for (int step = 0; step < 8; ++step) {
                        sendKey(ic.get(), fcitx::Key(FcitxKey_Down));
                    }
                    REQUIRE(pageable->currentPage() == 1);
                    REQUIRE(cl->size() == 4);
                    REQUIRE(cl->cursorIndex() == 0);
                    REQUIRE(cl->candidate(0).text().toString() == "Russian");
                    REQUIRE(cl->candidate(0).customLabel().toString() == "1. ");
                    REQUIRE(cl->candidate(3).customLabel().toString() == "4. ");

                    // Up from page 1 index 0 returns to page 0 index 8.
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Up));
                    REQUIRE(pageable->currentPage() == 0);
                    REQUIRE(cl->cursorIndex() == 8);

                    // Down back to page 1 index 0, then Return selects Russian.
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Down));
                    REQUIRE(pageable->currentPage() == 1);
                    REQUIRE(cl->cursorIndex() == 0);
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
                });
                {
                    auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                        daemon.waitForCommand(cmdIdx++));
                    REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                            (*cmd)["action"] == "translate_ru");
                }
                daemon.sendEvent(R"({"event":"final_result","text":"Привет"})");
                REQUIRE(waitOnMain(
                    instance, [&]() { return ic->committed.size() == 9; }));
                REQUIRE(ic->committed[8] == "Привет");

                // Test digit selection on page 2 (Page_Down + '2' -> Italian)
                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key("Alt+space"));
                });
                daemon.waitForCommand(cmdIdx++);
                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
                });
                daemon.waitForCommand(cmdIdx++);
                daemon.sendEvent(twelveTargetsJson);
                REQUIRE(waitOnMain(instance, [&]() {
                    auto cl = ic->inputPanel().candidateList();
                    return cl && cl->size() == 5;
                }));
                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key(FcitxKey_3));
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Page_Down));
                    auto cl = ic->inputPanel().candidateList();
                    REQUIRE(cl != nullptr);
                    REQUIRE(cl->toPageable()->currentPage() == 1);
                    REQUIRE(cl->cursorIndex() == 0);

                    // Page_Up returns to page 0 and skips the header at index
                    // 0.
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Page_Up));
                    REQUIRE(cl->toPageable()->currentPage() == 0);
                    REQUIRE(cl->cursorIndex() == 1);

                    // Page_Down back to page 1 and select with digit '2'.
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Page_Down));
                    REQUIRE(cl->toPageable()->currentPage() == 1);
                    REQUIRE(cl->cursorIndex() == 0);
                    sendKey(ic.get(), fcitx::Key(FcitxKey_2));
                });
                {
                    auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                        daemon.waitForCommand(cmdIdx++));
                    REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                            (*cmd)["action"] == "translate_it");
                }
                daemon.sendEvent(R"({"event":"final_result","text":"Ciao"})");
                REQUIRE(waitOnMain(
                    instance, [&]() { return ic->committed.size() == 10; }));
                REQUIRE(ic->committed[9] == "Ciao");
            }

            // 7d. Disconnected daemon must NOT call ic->reset() on trigger
            {
                std::atomic<int> resetEvents{0};
                auto resetWatcher =
                    instance.watchEvent(fcitx::EventType::InputContextReset,
                                        fcitx::EventWatcherPhase::Default,
                                        [&](fcitx::Event &) { ++resetEvents; });
                const std::string badSockPath =
                    (root / "nonexistent.sock").string();
                runOnMain(instance, [&]() {
                    fcitx::RawConfig rawCfg;
                    rawCfg.setValueByPath("SocketPath", badSockPath);
                    addon->setConfig(rawCfg);
                    ic->inputPanel().setClientPreedit(fcitx::Text("nihao"));
                    sendKey(ic.get(), fcitx::Key("Alt+space"));
                    REQUIRE(resetEvents.load() == 0);
                    REQUIRE(ic->inputPanel().clientPreedit().toString() ==
                            "nihao");
                    ic->inputPanel().reset();
                    rawCfg.setValueByPath("SocketPath", sockPath);
                    addon->setConfig(rawCfg);
                });
            }

            // 7e. Explicit TranslateLanguages=["en"] overrides daemon targets
            {
                runOnMain(instance, [&]() {
                    fcitx::RawConfig rawCfg;
                    rawCfg.setValueByPath("SocketPath", sockPath);
                    rawCfg.setValueByPath("TranslateLanguages/0", "en");
                    addon->setConfig(rawCfg);
                    sendKey(ic.get(), fcitx::Key("Alt+space"));
                });
                daemon.waitForCommand(cmdIdx++);
                runOnMain(instance, [&]() {
                    sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
                });
                daemon.waitForCommand(cmdIdx++);
                daemon.sendEvent(
                    R"({"event":"transcribe_done","raw_text":"你好","translate_targets":[{"action":"translate_en","label":"English"},{"action":"translate_ja","label":"Japanese"}]})");
                REQUIRE(waitOnMain(instance, [&]() {
                    auto cl = ic->inputPanel().candidateList();
                    return cl && cl->size() == 4;
                }));
                runOnMain(instance, [&]() {
                    auto cl = ic->inputPanel().candidateList();
                    REQUIRE(cl->candidate(2).text().toString() ==
                            "[Translate] Translate to English");
                    sendKey(ic.get(), fcitx::Key(FcitxKey_3));
                });
                {
                    auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                        daemon.waitForCommand(cmdIdx++));
                    REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                            (*cmd)["action"] == "translate_en");
                }
                daemon.sendEvent(R"({"event":"final_result","text":"Hello"})");
                REQUIRE(waitOnMain(
                    instance, [&]() { return ic->committed.size() == 11; }));
                REQUIRE(ic->committed[10] == "Hello");
                runOnMain(instance, [&]() {
                    fcitx::RawConfig rawCfg;
                    rawCfg.setValueByPath("SocketPath", sockPath);
                    addon->setConfig(rawCfg);
                });
            }

            // 7f. Full-schema save: every daemon-consumed key must survive the
            // addon's own save path. The resulting conf is compared against the
            // golden file the Go daemon's loader tests parse, so both sides
            // provably agree on the real Fcitx5 serialization format.
            {
                const fs::path confPath =
                    configDir / "fcitx5" / "conf" / "gemini-voice.conf";
                runOnMain(instance, [&]() {
                    fcitx::RawConfig rawCfg;
                    rawCfg.setValueByPath("TriggerKey/0", "Alt+space");
                    rawCfg.setValueByPath("TriggerKey/1", "Control+F12");
                    rawCfg.setValueByPath("ShowActionMenu", "True");
                    rawCfg.setValueByPath("SocketPath",
                                          "$XDG_RUNTIME_DIR/gemini-ime.sock");
                    rawCfg.setValueByPath("Language", "cmn-Hans-CN,en-US");
                    rawCfg.setValueByPath("RecordingPrefix", "🎙️");
                    rawCfg.setValueByPath("PreeditMaxWidth", "80");
                    rawCfg.setValueByPath("TranslateLanguages/0", "en");
                    rawCfg.setValueByPath("TranslateLanguages/1", "Pig Latin");
                    rawCfg.setValueByPath("LiveTranscription/Model",
                                          "gemini-3.5-transcribe-live");
                    rawCfg.setValueByPath("LiveTranscription/Mode", "SMART");
                    rawCfg.setValueByPath(
                        "LiveTranscription/CustomVocabulary/0", "Fcitx5");
                    rawCfg.setValueByPath(
                        "LiveTranscription/CustomVocabulary/1", "Pipe Wire");
                    rawCfg.setValueByPath("LiveTranscription/VadSilenceMs",
                                          "1500");
                    rawCfg.setValueByPath("PostProcess/Model",
                                          "gemini-3.5-flash/low");
                    rawCfg.setValueByPath("PostProcess/RefineFixTypos/Prefetch",
                                          "False");
                    rawCfg.setValueByPath("PostProcess/RefineFormal/Model",
                                          "gemini-4-hydrogen/high");
                    rawCfg.setValueByPath(
                        "PostProcess/RefineFormal/Prompt",
                        "Line one \"quoted\" # not a comment\n"
                        "  line two with \\ backslash: 中文");
                    rawCfg.setValueByPath("PostProcess/Translate/Model",
                                          "gemini-3.5-flash");
                    rawCfg.setValueByPath("PostProcess/Translate/Prefetch",
                                          "True");
                    rawCfg.setValueByPath("PostProcess/Actions/0/Name",
                                          "translate_ja");
                    rawCfg.setValueByPath("PostProcess/Actions/0/Model",
                                          "gemini-3.5-flash/low");
                    // Not in the dropdown (custom translation language): the
                    // value must survive setConfig untouched.
                    rawCfg.setValueByPath("PostProcess/Actions/1/Name",
                                          "translate_pig_latin");
                    rawCfg.setValueByPath("PostProcess/Actions/1/Prompt",
                                          "Translate to Pig Latin.");
                    rawCfg.setValueByPath("Connection/Proxy",
                                          "socks5://127.0.0.1:1080");
                    rawCfg.setValueByPath("Connection/LiveWsUrl",
                                          "wss://example.invalid/live");
                    rawCfg.setValueByPath("Connection/ApiBaseUrl",
                                          "https://example.invalid/v1beta");
                    addon->setConfig(rawCfg);
                });
                std::ifstream in(confPath);
                REQUIRE(in.good());
                std::stringstream saved;
                saved << in.rdbuf();
                if (::getenv("UPDATE_GOLDEN")) {
                    std::ofstream out(GOLDEN_CONF_PATH, std::ios::binary);
                    out << saved.str();
                }
                std::ifstream gin(GOLDEN_CONF_PATH);
                REQUIRE(gin.good());
                std::stringstream expected;
                expected << gin.rdbuf();
                if (saved.str() != expected.str()) {
                    std::fprintf(stderr,
                                 "golden conf mismatch (rerun with "
                                 "UPDATE_GOLDEN=1 to regenerate)\n--- "
                                 "saved\n%s--- golden\n%s",
                                 saved.str().c_str(), expected.str().c_str());
                }
                REQUIRE(saved.str() == expected.str());
                runOnMain(instance, [&]() {
                    fcitx::RawConfig rawCfg;
                    rawCfg.setValueByPath("TranslateLanguages/0", "en");
                    rawCfg.setValueByPath("SocketPath", sockPath);
                    addon->setConfig(rawCfg);
                });
            }

            // 8. Unload safety: capture CandidateList before engine teardown
            runOnMain(instance, [&]() {
                fcitx::RawConfig rawCfg;
                rawCfg.setValueByPath("ShowActionMenu", "True");
                rawCfg.setValueByPath("PreeditMaxWidth", "80");
                addon->setConfig(rawCfg);
                sendKey(ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"卸载安全测试"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance, [&]() {
                staleList = ic->inputPanel().candidateList();
                REQUIRE(staleList != nullptr);
                ic.reset();
                instance.exit();
            });
        });

        instance.eventLoop().exec();
        driver.join();
    }

    REQUIRE(staleList != nullptr);
    staleList->candidate(0).select(nullptr);
    staleList->candidate(1).select(nullptr);
    staleList->candidate(2).select(nullptr);
    staleList->candidate(3).select(nullptr);
    staleList.reset();

    daemon.stop();
    std::error_code ec;
    fs::remove_all(root, ec);

    std::cout << "test_engine_integration PASSED\n";
    return 0;
}
