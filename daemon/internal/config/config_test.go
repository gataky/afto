package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadOverridesAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("latency_budget_ms = 25\n[providers]\nfrecency = false\n"), 0o600)

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LatencyBudgetMS != 25 || c.Providers.Frecency || !c.Providers.History {
		t.Fatalf("got %+v", c)
	}
	if c.IdleShutdownMin != 30 || c.LogLevel != "info" {
		t.Fatalf("unset keys must keep defaults: %+v", c)
	}
}

func TestLoadInvalidFileReturnsDefaultsAndError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("latency_budget_ms = }{"), 0o600)

	c, err := Load(p)
	if err == nil {
		t.Fatal("want parse error")
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("invalid file must yield defaults, got %+v", c)
	}
}

func TestNonsenseValuesFallBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("latency_budget_ms = -5\nidle_shutdown_min = 0\n"), 0o600)

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LatencyBudgetMS != 40 || c.IdleShutdownMin != 30 {
		t.Fatalf("got %+v", c)
	}
}

func TestManagerReloadSwapsAndNotifies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	m := NewManager(p, log) // file absent → defaults
	if got := m.Get().LatencyBudgetMS; got != 40 {
		t.Fatalf("got %d", got)
	}

	notified := make(chan Config, 1)
	m.OnReload(func(c Config) { notified <- c })

	os.WriteFile(p, []byte("latency_budget_ms = 15\n"), 0o600)
	m.Reload()
	if got := m.Get().LatencyBudgetMS; got != 15 {
		t.Fatalf("got %d", got)
	}
	if c := <-notified; c.LatencyBudgetMS != 15 {
		t.Fatalf("callback got %+v", c)
	}

	// Invalid edit keeps previous config live.
	os.WriteFile(p, []byte("}{"), 0o600)
	m.Reload()
	if got := m.Get().LatencyBudgetMS; got != 15 {
		t.Fatalf("invalid reload must keep previous, got %d", got)
	}
}
