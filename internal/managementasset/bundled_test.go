package managementasset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBundledPanelNeverDownloadsOrOverwrites(t *testing.T) {
	t.Setenv("MANAGEMENT_PANEL_BUNDLED", "true")
	dir := t.TempDir()
	if _, skip := autoUpdateSkipReason(&config.Config{}); !skip {
		t.Fatal("bundled UI would be auto-updated")
	}
	if EnsureLatestManagementHTML(t.Context(), dir, "", "http://127.0.0.1:1/unreachable") {
		t.Fatal("missing bundle must fail visibly")
	}
	path := filepath.Join(dir, ManagementFileName)
	if err := os.WriteFile(path, []byte("custom panel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !EnsureLatestManagementHTML(t.Context(), dir, "", "http://127.0.0.1:1/unreachable") {
		t.Fatal("existing bundle not served")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "custom panel" {
		t.Fatalf("bundle overwritten: %q %v", data, err)
	}
}
