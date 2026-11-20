# ==============================================================================
# Fcitx5 Gemini Voice IME Justfile
# ==============================================================================

# List all available recipes
default:
    @just --list

# Build version: UTC build time and short commit (falls back to nogit)
version := `date -u +%Y-%m-%d-%H-%M-%S` + ":" + `git rev-parse --short=8 HEAD 2>/dev/null || echo nogit`

# Format Go and C++ source files
fmt:
    goimports -w -local "github.com/elmhuangyu/fcitx5-gemini-asr/daemon" daemon
    clang-format -i addon/*.h addon/*.cpp addon/tests/*.cpp

# Verify Go and C++ formatting without modifying files
fmt-check:
    @unformatted="$(goimports -local "github.com/elmhuangyu/fcitx5-gemini-asr/daemon" -l daemon)"; \
    if [ -n "$unformatted" ]; then \
        echo "Unformatted Go files:"; \
        echo "$unformatted"; \
        exit 1; \
    fi
    clang-format --dry-run --Werror addon/*.h addon/*.cpp addon/tests/*.cpp

# Tidy Go module dependencies
tidy:
    cd daemon && go mod tidy

# Update Go module dependencies
update-deps:
    cd daemon && go get -u -t ./... && go mod tidy

# Run linters for Go daemon, shell installer, and C++ addon (-Werror)
lint:
    cd daemon && golangci-lint run ./...
    bash -n install.sh
    cmake -S addon -B addon/build -DCMAKE_BUILD_TYPE=Release -DENABLE_TEST=ON -DCMAKE_CXX_FLAGS="-Werror"
    cmake --build addon/build -j"$(nproc)"

# Build Go daemon/CLI binary and gemini-voice symlink
build-daemon:
    cd daemon && go build -ldflags "-X main.Version={{version}}" -o gemini-voice-daemon .
    ln -sfn gemini-voice-daemon daemon/gemini-voice

# Build Fcitx5 C++ addon and test binaries
build-addon:
    cmake -S addon -B addon/build -DCMAKE_BUILD_TYPE=Release -DENABLE_TEST=ON -DCMAKE_CXX_FLAGS="-Werror"
    cmake --build addon/build -j"$(nproc)"

# Build both daemon/CLI and Fcitx5 addon
build: build-daemon build-addon

# Run Go daemon and CLI tests with race detector
test-daemon:
    cd daemon && go test -v -race ./...

# Run C++ unit and Fcitx5 integration tests
test-addon: build-addon
    ctest --test-dir addon/build --output-on-failure

# Run all tests
test: test-daemon test-addon

# Run full verification suite (format check, lint, build, and tests)
check: fmt-check lint build test

# Install user-local Fcitx5 addon (~/.local) and user daemon/service
install:
    ./install.sh

# Remove build artifacts
clean:
    rm -rf addon/build daemon/gemini-voice-daemon daemon/gemini-voice
