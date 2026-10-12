// Package cli shares classifier configuration and defaults between commands.
package cli

import (
	"fmt"

	"github.com/BurntSushi/toml"
	"typemail"
)

const (
	DefaultConcurrency   = 5
	DefaultMaxEmailBytes = 16 << 10
)

// LoadClassifierConfig strictly decodes and validates an optional TOML file.
func LoadClassifierConfig(path string) (typemail.ClassifierConfig, error) {
	var cfg typemail.ClassifierConfig
	if path != "" {
		metadata, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return cfg, fmt.Errorf("decode config %q: %w", path, err)
		}
		if keys := metadata.Undecoded(); len(keys) > 0 {
			return cfg, fmt.Errorf("config %q: unknown keys: %v", path, keys)
		}
		// TOML struct decoding ignores case, but setting names must match exactly.
		for _, key := range metadata.Keys() {
			if len(key) == 1 && key[0] == "skip_sender_domains" {
				continue
			}
			if key[0] != "classes" || (len(key) >= 3 && key[2] != "enabled" && key[2] != "criteria") {
				return cfg, fmt.Errorf("config %q: unknown key: %s", path, key)
			}
		}
	}
	// Reuse the classifier's semantic validation without making any requests.
	if _, err := typemail.NewClassifierWithConfig("config-validation", cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
