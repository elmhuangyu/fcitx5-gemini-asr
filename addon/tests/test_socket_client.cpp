#include "socket_client.h"

#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <iostream>
#include <mutex>
#include <poll.h>
#include <string>
#include <sys/socket.h>
#include <sys/un.h>
#include <thread>
#include <unistd.h>
#include <vector>

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

bool writeLine(int fd, const std::string &line) {
    std::string payload = line + "\n";
    size_t sent = 0;
    while (sent < payload.size()) {
        ssize_t n = ::send(fd, payload.data() + sent, payload.size() - sent,
                           MSG_NOSIGNAL);
        if (n > 0) {
            sent += static_cast<size_t>(n);
            continue;
        }
        if (n < 0 && errno == EINTR) {
            continue;
        }
        return false;
    }
    return true;
}

void testNoWakeupPipeStopWithoutConnect() {
    std::atomic<int> msgCount{0};
    std::atomic<int> disconnectCount{0};
    fcitx_gemini_voice::SocketClient client(
        "/tmp/nonexistent-fcitx5-gemini-sock.sock",
        [&](const std::string &) { ++msgCount; }, [&]() { ++disconnectCount; },
        false);

    std::this_thread::sleep_for(std::chrono::milliseconds(20));

    auto start = std::chrono::steady_clock::now();
    client.stop();
    auto elapsedMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                         std::chrono::steady_clock::now() - start)
                         .count();
    REQUIRE(elapsedMs < 500);
    REQUIRE(msgCount.load() == 0);
    REQUIRE(disconnectCount.load() == 0);
}

void testNoWakeupPipeActiveSocketEcho() {
    char tmpl[] = "/tmp/fcitx5-gemini-socktest-XXXXXX";
    char *tmpDirPtr = ::mkdtemp(tmpl);
    REQUIRE(tmpDirPtr != nullptr);
    fs::path dir(tmpDirPtr);
    std::string sockPath = (dir / "daemon.sock").string();

    int listenFd = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    REQUIRE(listenFd >= 0);

    sockaddr_un addr{};
    REQUIRE(sockPath.size() < sizeof(addr.sun_path));
    addr.sun_family = AF_UNIX;
    std::memcpy(addr.sun_path, sockPath.c_str(), sockPath.size() + 1);
    REQUIRE(::bind(listenFd, reinterpret_cast<sockaddr *>(&addr),
                   sizeof(addr)) == 0);
    REQUIRE(::listen(listenFd, 2) == 0);

    std::string receivedCmd;
    std::atomic<bool> serverStop{false};
    std::thread serverThread([&]() {
        pollfd lpfd{listenFd, POLLIN, 0};
        int arc = ::poll(&lpfd, 1, 2000);
        REQUIRE(arc > 0);
        int connFd = ::accept4(listenFd, nullptr, nullptr, SOCK_CLOEXEC);
        REQUIRE(connFd >= 0);

        std::string buf;
        while (true) {
            pollfd cpfd{connFd, POLLIN, 0};
            int rc = ::poll(&cpfd, 1, 2000);
            REQUIRE(rc > 0);
            char tmp[256];
            ssize_t n = ::recv(connFd, tmp, sizeof(tmp), 0);
            if (n < 0 && errno == EINTR) {
                continue;
            }
            REQUIRE(n > 0);
            buf.append(tmp, static_cast<size_t>(n));
            size_t nl = buf.find('\n');
            if (nl != std::string::npos) {
                receivedCmd = buf.substr(0, nl);
                break;
            }
        }

        REQUIRE(writeLine(connFd, R"({"event":"echo","ok":"true"})"));

        while (!serverStop.load()) {
            pollfd cpfd{connFd, POLLIN, 0};
            int rc = ::poll(&cpfd, 1, 50);
            if (rc > 0) {
                char tmp[64];
                ssize_t n = ::recv(connFd, tmp, sizeof(tmp), MSG_DONTWAIT);
                if (n == 0 || (n < 0 && errno != EAGAIN &&
                               errno != EWOULDBLOCK && errno != EINTR)) {
                    break;
                }
            }
        }
        ::close(connFd);
    });

    std::mutex mu;
    std::condition_variable cv;
    std::vector<std::string> messages;
    fcitx_gemini_voice::SocketClient client(
        sockPath,
        [&](const std::string &line) {
            std::lock_guard<std::mutex> lock(mu);
            messages.push_back(line);
            cv.notify_all();
        },
        []() {}, false);

    REQUIRE(client.sendLine(R"({"cmd":"ping"})"));

    {
        std::unique_lock<std::mutex> lock(mu);
        bool gotMsg = cv.wait_for(lock, std::chrono::milliseconds(1000),
                                  [&]() { return !messages.empty(); });
        REQUIRE(gotMsg);
        REQUIRE(messages.size() == 1);
        REQUIRE(messages[0] == R"({"event":"echo","ok":"true"})");
    }

    auto start = std::chrono::steady_clock::now();
    client.stop();
    auto elapsedMs = std::chrono::duration_cast<std::chrono::milliseconds>(
                         std::chrono::steady_clock::now() - start)
                         .count();
    REQUIRE(elapsedMs < 500);

    serverStop.store(true);
    serverThread.join();
    ::close(listenFd);

    REQUIRE(receivedCmd == R"({"cmd":"ping"})");

    std::error_code ec;
    fs::remove_all(dir, ec);
}

void testSetSocketPathNotifiesDisconnect() {
    char tmpl[] = "/tmp/fcitx5-gemini-sockpath-XXXXXX";
    char *tmpDirPtr = ::mkdtemp(tmpl);
    REQUIRE(tmpDirPtr != nullptr);
    fs::path dir(tmpDirPtr);
    std::string sockPathA = (dir / "a.sock").string();
    std::string sockPathB = (dir / "b.sock").string();

    int listenFd = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    REQUIRE(listenFd >= 0);

    sockaddr_un addr{};
    REQUIRE(sockPathA.size() < sizeof(addr.sun_path));
    addr.sun_family = AF_UNIX;
    std::memcpy(addr.sun_path, sockPathA.c_str(), sockPathA.size() + 1);
    REQUIRE(::bind(listenFd, reinterpret_cast<sockaddr *>(&addr),
                   sizeof(addr)) == 0);
    REQUIRE(::listen(listenFd, 2) == 0);

    std::mutex mu;
    std::condition_variable cv;
    bool accepted = false;
    bool peerClosed = false;
    std::atomic<bool> serverStop{false};
    std::thread serverThread([&]() {
        pollfd lpfd{listenFd, POLLIN, 0};
        int arc = ::poll(&lpfd, 1, 2000);
        REQUIRE(arc > 0);
        int connFd = ::accept4(listenFd, nullptr, nullptr, SOCK_CLOEXEC);
        REQUIRE(connFd >= 0);
        {
            std::lock_guard<std::mutex> lock(mu);
            accepted = true;
            cv.notify_all();
        }
        while (!serverStop.load()) {
            pollfd cpfd{connFd, POLLIN, 0};
            int rc = ::poll(&cpfd, 1, 100);
            if (rc <= 0) {
                continue;
            }
            char tmp[256];
            ssize_t n = ::recv(connFd, tmp, sizeof(tmp), MSG_DONTWAIT);
            if (n < 0 && errno == EINTR) {
                continue;
            }
            if (n <= 0) {
                std::lock_guard<std::mutex> lock(mu);
                peerClosed = true;
                cv.notify_all();
                break;
            }
        }
        ::close(connFd);
    });

    std::atomic<int> disconnectCount{0};
    fcitx_gemini_voice::SocketClient client(
        sockPathA, [](const std::string &) {}, [&]() { ++disconnectCount; },
        true);

    // Establish the live connection.
    REQUIRE(client.sendLine(R"({"cmd":"ping"})"));
    {
        std::unique_lock<std::mutex> lock(mu);
        bool ok = cv.wait_for(lock, std::chrono::milliseconds(1000),
                              [&]() { return accepted; });
        REQUIRE(ok);
    }

    // Re-setting the same path must not disturb the live connection.
    client.setSocketPath(sockPathA);
    REQUIRE(client.socketPath() == sockPathA);
    REQUIRE(disconnectCount.load() == 0);

    // Changing the path retires the live connection and must deliver the
    // disconnect notification, like a reader-loop EOF would.
    client.setSocketPath(sockPathB);
    REQUIRE(client.socketPath() == sockPathB);
    REQUIRE(disconnectCount.load() == 1);

    // The retired connection is really torn down server-side.
    {
        std::unique_lock<std::mutex> lock(mu);
        bool ok = cv.wait_for(lock, std::chrono::milliseconds(1000),
                              [&]() { return peerClosed; });
        REQUIRE(ok);
    }

    // The reader loop must not deliver a second notification for the
    // retired fd.
    std::this_thread::sleep_for(std::chrono::milliseconds(150));
    REQUIRE(disconnectCount.load() == 1);

    client.stop();
    serverStop.store(true);
    serverThread.join();
    ::close(listenFd);

    std::error_code ec;
    fs::remove_all(dir, ec);
}

} // namespace

int main() {
    testNoWakeupPipeStopWithoutConnect();
    testNoWakeupPipeActiveSocketEcho();
    testSetSocketPathNotifiesDisconnect();
    std::cout << "test_socket_client PASSED\n";
    return 0;
}
