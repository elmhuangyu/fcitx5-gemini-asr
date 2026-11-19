#include "socket_client.h"

#include <cerrno>
#include <cstring>
#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

namespace fcitx_gemini_voice {

namespace {
constexpr size_t kMaxLineBytes = 4 * 1024 * 1024;
} // namespace

SocketClient::SocketClient(std::string socketPath, MessageCallback onMessage,
                           DisconnectCallback onDisconnect,
                           bool enableWakeupPipe)
    : onMessage_(std::move(onMessage)), onDisconnect_(std::move(onDisconnect)),
      socketPath_(std::move(socketPath)) {
    if (enableWakeupPipe && ::pipe2(wakeupFds_, O_CLOEXEC | O_NONBLOCK) != 0) {
        wakeupFds_[0] = -1;
        wakeupFds_[1] = -1;
    }
    readerThread_ = std::thread(&SocketClient::readerLoop, this);
}

SocketClient::~SocketClient() { stop(); }

void SocketClient::stop() {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (stopRequested_) {
            return;
        }
        stopRequested_ = true;
        if (fd_ >= 0) {
            retireFdLocked(fd_);
            fd_ = -1;
        }
    }
    wakeReader();
    if (readerThread_.joinable()) {
        readerThread_.join();
    }

    std::lock_guard<std::mutex> lock(mutex_);
    for (int fd : fdsToClose_) {
        if (fd >= 0) {
            ::close(fd);
        }
    }
    fdsToClose_.clear();
    if (fd_ >= 0) {
        ::close(fd_);
        fd_ = -1;
    }
    if (wakeupFds_[0] >= 0) {
        ::close(wakeupFds_[0]);
        wakeupFds_[0] = -1;
    }
    if (wakeupFds_[1] >= 0) {
        ::close(wakeupFds_[1]);
        wakeupFds_[1] = -1;
    }
}

void SocketClient::setSocketPath(std::string path) {
    bool changed = false;
    bool notifyDisconnect = false;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (socketPath_ != path) {
            socketPath_ = std::move(path);
            pathDirty_ = true;
            if (fd_ >= 0) {
                retireFdLocked(fd_);
                fd_ = -1;
                notifyDisconnect = !stopRequested_;
            }
            changed = true;
        }
    }
    if (changed) {
        wakeReader();
    }
    // A live connection was retired because the socket path changed; notify
    // through the same callback the reader loop uses on EOF/error so owners
    // observe the loss instead of staying stuck on the dead session.
    if (notifyDisconnect && onDisconnect_) {
        onDisconnect_();
    }
}

std::string SocketClient::socketPath() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return socketPath_;
}

std::string SocketClient::lastError() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return lastError_;
}

bool SocketClient::sendLine(const std::string &jsonLine) {
    std::lock_guard<std::mutex> writeLock(writeMutex_);
    std::string payload = jsonLine;
    payload.push_back('\n');

    for (int attempt = 0; attempt < 2; ++attempt) {
        int currentFd = -1;
        std::string pathCopy;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            if (stopRequested_) {
                lastError_ = "socket client stopped";
                return false;
            }
            if (pathDirty_ && fd_ >= 0) {
                retireFdLocked(fd_);
                fd_ = -1;
            }
            pathDirty_ = false;
            currentFd = fd_;
            pathCopy = socketPath_;
        }

        if (currentFd < 0) {
            std::string connErr;
            int newFd = connectUnix(pathCopy, connErr);
            if (newFd < 0) {
                std::lock_guard<std::mutex> lock(mutex_);
                lastError_ = std::move(connErr);
                return false;
            }
            {
                std::lock_guard<std::mutex> lock(mutex_);
                if (stopRequested_) {
                    ::close(newFd);
                    lastError_ = "socket client stopped";
                    return false;
                }
                if (fd_ >= 0) {
                    retireFdLocked(fd_);
                }
                fd_ = newFd;
                currentFd = newFd;
            }
            wakeReader();
        }

        int errNo = 0;
        if (writeAll(currentFd, payload.data(), payload.size(), errNo)) {
            return true;
        }

        {
            std::lock_guard<std::mutex> lock(mutex_);
            if (fd_ == currentFd) {
                retireFdLocked(fd_);
                fd_ = -1;
            }
            lastError_ = std::string("send failed: ") + std::strerror(errNo);
        }
        wakeReader();

        // Only transparently reconnect once if the existing socket had closed.
        if (errNo != EPIPE && errNo != ECONNRESET && errNo != EBADF) {
            return false;
        }
    }
    return false;
}

int SocketClient::connectUnix(const std::string &path, std::string &errOut) {
    if (path.empty()) {
        errOut = "socket path is empty";
        return -1;
    }
    sockaddr_un addr{};
    if (path.size() >= sizeof(addr.sun_path)) {
        errOut = "socket path exceeds AF_UNIX maximum length";
        return -1;
    }

    int fd = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd < 0) {
        errOut = std::string("socket() failed: ") + std::strerror(errno);
        return -1;
    }

    timeval tv{};
    tv.tv_sec = 0;
    tv.tv_usec = 500000;
    ::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv));

    addr.sun_family = AF_UNIX;
    std::memcpy(addr.sun_path, path.c_str(), path.size() + 1);

    if (::connect(fd, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) != 0) {
        errOut = std::string("connect(") + path +
                 ") failed: " + std::strerror(errno);
        ::close(fd);
        return -1;
    }
    return fd;
}

bool SocketClient::writeAll(int fd, const char *data, size_t len,
                            int &errNoOut) {
    size_t sent = 0;
    while (sent < len) {
        ssize_t n = ::send(fd, data + sent, len - sent, MSG_NOSIGNAL);
        if (n > 0) {
            sent += static_cast<size_t>(n);
            continue;
        }
        if (n < 0 && errno == EINTR) {
            continue;
        }
        errNoOut = (n < 0) ? errno : EPIPE;
        return false;
    }
    return true;
}

void SocketClient::retireFdLocked(int fd) {
    if (fd >= 0) {
        ::shutdown(fd, SHUT_RDWR);
        fdsToClose_.push_back(fd);
    }
}

void SocketClient::wakeReader() {
    if (wakeupFds_[1] >= 0) {
        char b = 1;
        ssize_t ignored = ::write(wakeupFds_[1], &b, 1);
        (void)ignored;
    }
}

void SocketClient::drainWakeup() {
    if (wakeupFds_[0] < 0) {
        return;
    }
    char buf[64];
    while (::read(wakeupFds_[0], buf, sizeof(buf)) > 0) {
    }
}

void SocketClient::readerLoop() {
    std::string lineBuf;
    int lastPolledFd = -1;

    while (true) {
        std::vector<int> toClose;
        int activeFd = -1;
        bool stopping = false;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            toClose.swap(fdsToClose_);
            activeFd = fd_;
            stopping = stopRequested_;
        }

        if (!toClose.empty()) {
            std::lock_guard<std::mutex> writeLock(writeMutex_);
            for (int oldFd : toClose) {
                if (oldFd >= 0) {
                    ::close(oldFd);
                }
                if (oldFd == lastPolledFd) {
                    lineBuf.clear();
                    lastPolledFd = -1;
                }
            }
        }

        if (stopping) {
            break;
        }
        if (activeFd != lastPolledFd) {
            lineBuf.clear();
            lastPolledFd = activeFd;
        }

        pollfd pfds[2]{};
        nfds_t nfds = 0;
        if (wakeupFds_[0] >= 0) {
            pfds[nfds].fd = wakeupFds_[0];
            pfds[nfds].events = POLLIN;
            ++nfds;
        }
        if (activeFd >= 0) {
            pfds[nfds].fd = activeFd;
            pfds[nfds].events = POLLIN;
            ++nfds;
        }

        constexpr int kFallbackPollTimeoutMs = 50;
        const int pollTimeoutMs =
            (wakeupFds_[0] >= 0) ? -1 : kFallbackPollTimeoutMs;
        int rc = ::poll((nfds > 0) ? pfds : nullptr, nfds, pollTimeoutMs);
        if (rc < 0) {
            if (errno == EINTR) {
                continue;
            }
            break;
        }
        if (rc == 0) {
            continue;
        }

        if (wakeupFds_[0] >= 0 && (pfds[0].revents & POLLIN)) {
            drainWakeup();
        }

        if (activeFd < 0) {
            continue;
        }

        const pollfd &sockPfd = (wakeupFds_[0] >= 0) ? pfds[1] : pfds[0];
        if (sockPfd.revents == 0) {
            continue;
        }

        bool disconnected = false;
        char buf[4096];
        while (true) {
            ssize_t n = ::recv(activeFd, buf, sizeof(buf), MSG_DONTWAIT);
            if (n > 0) {
                if (lineBuf.size() + static_cast<size_t>(n) > kMaxLineBytes) {
                    lineBuf.clear();
                    disconnected = true;
                    break;
                }
                lineBuf.append(buf, static_cast<size_t>(n));
                size_t start = 0;
                while (true) {
                    size_t nl = lineBuf.find('\n', start);
                    if (nl == std::string::npos) {
                        break;
                    }
                    std::string line = lineBuf.substr(start, nl - start);
                    if (!line.empty() && line.back() == '\r') {
                        line.pop_back();
                    }
                    if (!line.empty() && onMessage_) {
                        onMessage_(line);
                    }
                    start = nl + 1;
                }
                if (start > 0) {
                    lineBuf.erase(0, start);
                }
            } else if (n == 0) {
                disconnected = true;
                break;
            } else {
                if (errno == EINTR) {
                    continue;
                }
                if (errno == EAGAIN || errno == EWOULDBLOCK) {
                    break;
                }
                disconnected = true;
                break;
            }
        }

        if (disconnected ||
            (sockPfd.revents & (POLLERR | POLLHUP | POLLNVAL))) {
            bool notifyDisconnect = false;
            {
                std::lock_guard<std::mutex> lock(mutex_);
                if (fd_ == activeFd) {
                    retireFdLocked(fd_);
                    fd_ = -1;
                    notifyDisconnect = !stopRequested_;
                }
            }
            lineBuf.clear();
            if (notifyDisconnect && onDisconnect_) {
                onDisconnect_();
            }
        }
    }
}

} // namespace fcitx_gemini_voice
