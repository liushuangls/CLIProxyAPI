package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUsageHistoryV8RoundTrip(t *testing.T) {
	data := []byte("observability:\n  usage:\n    history:\n      disabled: true\n      path: /data/history.db\n      retention-days: 7\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UsageHistory.Disabled || cfg.UsageHistory.RetentionDays != 7 || cfg.UsageHistory.Path != "/data/history.db" {
		t.Fatalf("lost settings: %+v", cfg.UsageHistory)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.UsageHistory != cfg.UsageHistory {
		t.Fatalf("round trip changed history: %v", err)
	}
	if _, err = ParseConfigBytes([]byte("observability:\n  usage:\n    history:\n      retention-days: -1\n")); err == nil {
		t.Fatal("negative retention accepted")
	}
}
