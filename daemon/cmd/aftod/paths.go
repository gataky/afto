package main

import (
	"os"
	"path/filepath"
)

// Path resolution (plans/phase-1.md §6, §8): explicit env override first,
// then XDG, then a home-relative fallback. The fallbacks matter on macOS,
// which sets none of the XDG variables by default.

// socketPath: $AFTO_SOCKET → $XDG_RUNTIME_DIR/afto/afto.sock →
// ~/.cache/afto/afto.sock. The parent directory is created 0700 by the
// daemon: the socket is a private channel to the user's command history.
func socketPath() string {
	if p := os.Getenv("AFTO_SOCKET"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "afto", "afto.sock")
	}
	return filepath.Join(home(), ".cache", "afto", "afto.sock")
}

// dataDir holds afto.db: $AFTO_DATA_DIR → $XDG_DATA_HOME/afto →
// ~/.local/share/afto.
func dataDir() string {
	if p := os.Getenv("AFTO_DATA_DIR"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "afto")
	}
	return filepath.Join(home(), ".local", "share", "afto")
}

// configPath: $AFTO_CONFIG → $XDG_CONFIG_HOME/afto/config.toml →
// ~/.config/afto/config.toml.
func configPath() string {
	if p := os.Getenv("AFTO_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "afto", "config.toml")
	}
	return filepath.Join(home(), ".config", "afto", "config.toml")
}

// logPath: $XDG_STATE_HOME/afto/aftod.log → ~/.local/state/afto/aftod.log.
func logPath() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "afto", "aftod.log")
	}
	return filepath.Join(home(), ".local", "state", "afto", "aftod.log")
}

// histfilePath finds the history file to bootstrap-import from:
// $HISTFILE → ~/.zsh_history → ~/.zhistory; "" when none exists.
func histfilePath() string {
	if p := os.Getenv("HISTFILE"); p != "" {
		return p
	}
	for _, name := range []string{".zsh_history", ".zhistory"} {
		p := filepath.Join(home(), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
