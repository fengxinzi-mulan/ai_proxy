package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCloseAction(t *testing.T) {
	cases := []struct {
		in   CloseAction
		want CloseAction
	}{
		{CloseAsk, CloseAsk},
		{CloseQuit, CloseQuit},
		{CloseTray, CloseTray},
		{legacyCloseMinimize, CloseTray},
		{"", CloseAsk},
		{"unknown", CloseAsk},
	}
	for _, tc := range cases {
		c := Config{CloseAction: tc.in}
		c.normalize()
		if c.CloseAction != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, c.CloseAction, tc.want)
		}
	}
}

func TestLoadConfigMigratesLegacyMinimize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "desktop.json"), []byte(`{"closeAction":"minimize"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadConfig(dir)
	if got.CloseAction != CloseTray {
		t.Fatalf("legacy minimize = %q, want %q", got.CloseAction, CloseTray)
	}
}
