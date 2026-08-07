package sync

import (
	"testing"

	"github.com/RP2/gh-vault/internal/model"
)

func TestPathForFormatClone(t *testing.T) {
	got := PathForFormat(model.FormatClone, "RP2", "gh-vault")
	want := "active/RP2/gh-vault.git"
	if got != want {
		t.Errorf("PathForFormat(clone) = %q, want %q", got, want)
	}
}

func TestPathForFormatBundle(t *testing.T) {
	got := PathForFormat(model.FormatBundle, "RP2", "gh-vault")
	want := "archived/RP2/gh-vault.bundle"
	if got != want {
		t.Errorf("PathForFormat(bundle) = %q, want %q", got, want)
	}
}
