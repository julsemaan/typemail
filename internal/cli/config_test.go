package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadClassifierConfig(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		bad           bool
	}{
		{"defaults", "", false},
		{"custom", "[classes.receipts]\ncriteria = 'Invoices'", false},
		{"unknown", "[classes.spam]\nextra = true", true},
		{"case", "[classes.spam]\nEnabled = false", true},
		{"invalid criteria", "[classes.receipts]\ncriteria = ''", true},
		{"missing custom criteria", "[classes.receipts]\nenabled = false", true},
		{"invalid name", "[classes.' bad ']\ncriteria = 'Bad'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "classes.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadClassifierConfig(path)
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v, want bad=%v", err, tc.bad)
			}
		})
	}
	if _, err := LoadClassifierConfig(""); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClassifierConfig(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}
