package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func defaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gemini-voice", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".config", "gemini-voice", "config.toml")
	}
	return "config.toml"
}

func defaultSocketPath() string {
	if env := os.Getenv("GEMINI_IME_SOCKET"); env != "" {
		return env
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "gemini-ime.sock")
	}
	return fmt.Sprintf("/run/user/%d/gemini-ime.sock", os.Getuid())
}

func runDaemon(configPath, socketPath string) int {
	if migrated, migErr := MigrateOrphanedFcitxTranslateLanguages(DefaultFcitxConfigPath(), translateMigrationMarkerPath(configPath)); migErr != nil {
		log.Printf("migrate fcitx translate_languages: %v", migErr)
	} else if migrated {
		log.Printf("migrated orphaned [TranslateLanguages] 0=en from %s", DefaultFcitxConfigPath())
	}

	cfgMgr, err := NewConfigManager(configPath)
	if err != nil {
		log.Printf("init config: %v", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := cfgMgr.Watch(ctx); err != nil {
			log.Printf("config watcher stopped: %v", err)
		}
	}()

	srv := NewSocketServer(socketPath, SocketServerDeps{
		ConfigManager: cfgMgr,
	})

	log.Printf("gemini-voice-daemon listening on %s (config: %s)", socketPath, configPath)
	if err := srv.Serve(ctx); err != nil {
		log.Printf("socket server error: %v", err)
		return 1
	}
	log.Printf("gemini-voice-daemon exited cleanly")
	return 0
}

func main() {
	os.Exit(runCLI(os.Args[0], os.Args[1:], defaultCLIDeps()))
}
