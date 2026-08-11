package config

import "testing"

func TestConfigEmptyDefaultIsProviderNeutral(t *testing.T) {
	cfg := Config{SchemaVersion: 1}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate(%+v) error: %v", cfg, err)
	}
}
