#ifndef FCITX5_GEMINI_VOICE_SOCKET_CLIENT_H
#define FCITX5_GEMINI_VOICE_SOCKET_CLIENT_H

#include <functional>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace fcitx_gemini_voice {

class SocketClient {
public:
    using MessageCallback = std::function<void(const std::string &)>;
    using DisconnectCallback = std::function<void()>;

    SocketClient(std::string socketPath, MessageCallback onMessage,
                 DisconnectCallback onDisconnect, bool enableWakeupPipe = true);
    ~SocketClient();

    SocketClient(const SocketClient &) = delete;
    SocketClient &operator=(const SocketClient &) = delete;

    void setSocketPath(std::string path);
    std::string socketPath() const;

    bool sendLine(const std::string &jsonLine);
    std::string lastError() const;
    void stop();

private:
    int connectUnix(const std::string &path, std::string &errOut);
    bool writeAll(int fd, const char *data, size_t len, int &errNoOut);
    void retireFdLocked(int fd);
    void wakeReader();
    void drainWakeup();
    void readerLoop();

    MessageCallback onMessage_;
    DisconnectCallback onDisconnect_;

    mutable std::mutex mutex_;
    std::mutex writeMutex_;
    std::string socketPath_;
    std::string lastError_;
    int fd_ = -1;
    std::vector<int> fdsToClose_;
    bool pathDirty_ = false;
    bool stopRequested_ = false;

    int wakeupFds_[2] = {-1, -1};
    std::thread readerThread_;
};

} // namespace fcitx_gemini_voice

#endif // FCITX5_GEMINI_VOICE_SOCKET_CLIENT_H
