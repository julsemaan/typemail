// Package cli shares classifier configuration and defaults between commands.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"typemail/pkg/typemail"
	"typemail/pkg/typesafe"
)

const (
	DefaultConcurrency        = 5
	DefaultMaxEmailBytes      = 16 << 10
	DefaultOpenRouterEndpoint = "https://openrouter.ai/api/v1/systemone"
	DefaultJevModel           = "~typesafe/jev-latest"
)

// NewClassifier validates email policy before constructing the provider client.
func NewClassifier(apiKey string, cfg typemail.Config) (*typemail.Classifier, error) {
	if err := typemail.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("openrouter API key is required")
	}
	client, err := typesafe.New(typesafe.Config{
		Endpoint: DefaultOpenRouterEndpoint,
		Model:    DefaultJevModel,
		APIKey:   apiKey,
	})
	if err != nil {
		return nil, err
	}
	return typemail.New(client, cfg)
}

// LoadClassifierConfig strictly decodes and validates an optional TOML file.
func LoadClassifierConfig(path string) (typemail.Config, error) {
	var cfg typemail.Config
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
	if err := typemail.ValidateConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
