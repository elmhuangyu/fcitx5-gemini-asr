package main

import (
	"context"
	"path/filepath"
)

// migrateLegacyIfDefault runs the one-time migrations for confPath: the
// config.toml import (only for the default conf location, so custom paths never
// touch the user's real legacy config) and the conversion of the retired
// [PostProcess/Prefetch] list into per-group Prefetch switches (any path). It
// reloads Fcitx5 when the file changed.
func migrateLegacyIfDefault(confPath string, deps cliDeps, logf func(format string, args ...any)) {
	changed := false
	if filepath.Clean(confPath) == filepath.Clean(DefaultConfigPath()) {
		modified, err := MigrateLegacyConfig(legacyTOMLPath(), confPath)
		if err != nil {
			logf("migrate legacy config %s: %v", legacyTOMLPath(), err)
			return
		}
		if modified {
			logf("migrated settings from %s into %s (old file renamed to config.toml.migrated)", legacyTOMLPath(), confPath)
			changed = true
		}
	}
	modified, err := MigrateConfFile(confPath)
	if err != nil {
		logf("migrate config %s: %v", confPath, err)
		return
	}
	if modified {
		logf("converted [PostProcess/Prefetch] in %s into per-action Prefetch settings", confPath)
		changed = true
	}
	if changed {
		ReloadFcitx5Config(context.Background(), deps)
	}
}
