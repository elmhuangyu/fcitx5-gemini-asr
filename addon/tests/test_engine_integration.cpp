#include "json_util.h"

#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <filesystem>
#include <functional>
#include <iostream>
#include <memory>
#include <mutex>
#include <poll.h>
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
#include <fcitx/inputmethodengine.h>
#include <fcitx/inputmethodentry.h>
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
            pollfd pfds[2]{};
            nfds_t nfds = 0;
            pfds[nfds++] = {wakeFds_[0], POLLIN, 0};
            if (clientFd_ < 0) {
                pfds[nfds++] = {listenFd_, POLLIN, 0};
            } else {
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

            if (clientFd_ < 0 && (pfds[1].revents & POLLIN)) {
                int cfd = ::accept4(listenFd_, nullptr, nullptr, SOCK_CLOEXEC);
                if (cfd >= 0) {
                    std::lock_guard<std::mutex> lock(mu_);
                    clientFd_ = cfd;
                    cv_.notify_all();
                }
            } else if (clientFd_ >= 0 &&
                       (pfds[1].revents & (POLLIN | POLLHUP | POLLERR))) {
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

void sendKey(fcitx::InputMethodEngine *engine,
             const fcitx::InputMethodEntry *entry, fcitx::InputContext *ic,
             const fcitx::Key &key) {
    fcitx::KeyEvent ev(ic, key, false);
    engine->keyEvent(*entry, ev);
    REQUIRE(ev.filtered());
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
    fs::create_directories(dataDir / "fcitx5" / "inputmethod");
    fs::create_directories(configDir);
    fs::create_directories(dataHomeDir);
    fs::create_directories(runtimeDir);

    fs::copy_file(BUILD_ADDON_SO, addonDir / "libgemini-voice.so",
                  fs::copy_options::overwrite_existing);
    fs::copy_file(BUILD_ADDON_CONF,
                  dataDir / "fcitx5" / "addon" / "gemini-voice.conf",
                  fs::copy_options::overwrite_existing);
    fs::copy_file(BUILD_IM_CONF,
                  dataDir / "fcitx5" / "inputmethod" / "gemini-voice.conf",
                  fs::copy_options::overwrite_existing);

    ::setenv("FCITX_ADDON_DIRS", addonDir.c_str(), 1);
    ::setenv("XDG_DATA_DIRS", dataDir.c_str(), 1);
    ::setenv("XDG_CONFIG_HOME", configDir.c_str(), 1);
    ::setenv("XDG_DATA_HOME", dataHomeDir.c_str(), 1);
    ::setenv("XDG_RUNTIME_DIR", runtimeDir.c_str(), 1);

    std::string sockPath = (runtimeDir / "gemini-ime.sock").string();
    MockDaemonServer daemon(sockPath);

    char arg0[] = "test_engine_integration";
    char arg1[] = "--disable=all";
    char arg2[] = "--enable=gemini-voice";
    char *argv[] = {arg0, arg1, arg2};

    std::shared_ptr<fcitx::CandidateList> staleList;
    {
        fcitx::Instance instance(3, argv);
        instance.addonManager().registerDefaultLoader(nullptr);
        instance.initialize();
        instance.setRunning(true);

        std::thread driver([&]() {
            fcitx::AddonInstance *addon = nullptr;
            fcitx::InputMethodEngineV2 *engine = nullptr;
            const fcitx::InputMethodEntry *entry = nullptr;
            std::unique_ptr<TestInputContext> ic;

            runOnMain(instance, [&]() {
                addon = instance.addonManager().addon("gemini-voice", true);
                REQUIRE(addon != nullptr);
                engine = dynamic_cast<fcitx::InputMethodEngineV2 *>(addon);
                REQUIRE(engine != nullptr);
                entry = instance.inputMethodManager().entry("gemini-voice");
                REQUIRE(entry != nullptr);

                ic = std::make_unique<TestInputContext>(
                    instance.inputContextManager());
                ic->setCapabilityFlags(fcitx::CapabilityFlag::Preedit);
                ic->focusIn();
            });

            int cmdIdx = 0;

            // 1. TriggerKey (Alt+space) -> start_recording + clientPreedit
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "start_recording");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().clientPreedit().toString() ==
                            "[Recording...]");
                });
            }

            // 2. partial_text updates clientPreedit when
            // CapabilityFlag::Preedit is set
            daemon.sendEvent(R"({"event":"partial_text","text":"今天下午"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().clientPreedit().toString() ==
                       "今天下午";
            }));

            // 3. Return -> stop_recording -> transcribe_done -> ACTION_MENU ->
            // Space commits raw_text
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
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
                       cl->layoutHint() == fcitx::CandidateLayoutHint::Vertical;
            }));
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_space));
                REQUIRE(ic->committed.size() == 1);
                REQUIRE(ic->committed[0] == "今天下午三点我们开个会");
                REQUIRE(!ic->inputPanel().candidateList());
                REQUIRE(ic->inputPanel().clientPreedit().empty());
            });

            // 4. Fallback preedit (when CapabilityFlag::Preedit is unset) +
            // Level 2 Refine navigation
            runOnMain(instance, [&]() {
                ic->setCapabilityFlags(fcitx::CapabilityFlags{});
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
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
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"把这段话改得正式一点"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));

            // Right -> Level 2 Refine submenu (3 options), Left -> back to
            // Level 1, Right -> '3' -> refine_formal
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Right));
                auto cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 3);
                REQUIRE(ic->inputPanel().auxUp().toString() == "Refine ▸");

                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Left));
                cl = ic->inputPanel().candidateList();
                REQUIRE(cl && cl->size() == 4);

                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Right));
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_3));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "refine_formal");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().preedit().toString() ==
                            "[AI Processing...]");
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
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_3));
            });
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

            // 5b. Digit '4' sends post_process("as_prompt")
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"帮我写个脚本"})");
            REQUIRE(waitOnMain(instance, [&]() {
                auto cl = ic->inputPanel().candidateList();
                return cl && cl->size() == 4;
            }));
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_4));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "post_process" &&
                        (*cmd)["action"] == "as_prompt");
            }
            daemon.sendEvent(
                R"({"event":"final_result","text":"Prompt: Write a script..."})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 3; }));
            runOnMain(instance, [&]() {
                REQUIRE(ic->committed[2] == "Prompt: Write a script...");
            });

            // 6. Escape during RECORDING sends cancel and clears UI
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Escape));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "cancel");
                runOnMain(instance,
                          [&]() { REQUIRE(ic->inputPanel().empty()); });
            }

            // 6b. Daemon `state_changed`: echoes of engine-initiated
            // transitions are ignored; an unexpected daemon-side reset
            // (idle with no result) recovers to idle with feedback.
            runOnMain(instance, [&]() {
                ic->setCapabilityFlags(fcitx::CapabilityFlag::Preedit);
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
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
                return ic->inputPanel().clientPreedit().toString() ==
                       "仍在录音中";
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
            runOnMain(instance, [&]() { REQUIRE(ic->committed.size() == 3); });

            // A subsequent session works normally.
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            {
                auto cmd = fcitx_gemini_voice::parseFlatJsonObject(
                    daemon.waitForCommand(cmdIdx++));
                REQUIRE(cmd && (*cmd)["cmd"] == "start_recording");
                runOnMain(instance, [&]() {
                    REQUIRE(ic->inputPanel().clientPreedit().toString() ==
                            "[Recording...]");
                });
            }
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Escape));
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
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(R"({"event":"state_changed","state":"idle"})");
            REQUIRE(waitOnMain(instance, [&]() {
                return ic->inputPanel().clientPreedit().empty() &&
                       ic->inputPanel().preedit().empty() &&
                       !ic->inputPanel().candidateList();
            }));

            // 7. ShowActionMenu=false commits raw_text directly
            runOnMain(instance, [&]() {
                fcitx::RawConfig rawCfg;
                rawCfg.setValueByPath("ShowActionMenu", "False");
                addon->setConfig(rawCfg);
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
            });
            daemon.waitForCommand(cmdIdx++);
            daemon.sendEvent(
                R"({"event":"transcribe_done","raw_text":"直接上屏"})");
            REQUIRE(waitOnMain(instance,
                               [&]() { return ic->committed.size() == 4; }));
            runOnMain(instance,
                      [&]() { REQUIRE(ic->committed[3] == "直接上屏"); });

            // 8. Unload safety: capture CandidateList before engine teardown
            runOnMain(instance, [&]() {
                fcitx::RawConfig rawCfg;
                rawCfg.setValueByPath("ShowActionMenu", "True");
                addon->setConfig(rawCfg);
                sendKey(engine, entry, ic.get(), fcitx::Key("Alt+space"));
            });
            daemon.waitForCommand(cmdIdx++);
            runOnMain(instance, [&]() {
                sendKey(engine, entry, ic.get(), fcitx::Key(FcitxKey_Return));
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
