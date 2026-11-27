package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

func TestSystemPromptConfigAndReload(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptFile, []byte("Initial prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`{
		"listen": "127.0.0.1:8443",
		"mode": "hybrid",
		"llm_base_url": "http://127.0.0.1:8317/v1",
		"llm_model": "gemini-3-flash",
		"llm_system_prompt_file": "` + promptFile + `"
	}`)
	cfg, err := parseConfig(raw, false)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	got, err := cfg.resolveSystemPrompt()
	if err != nil || got != "Initial prompt" {
		t.Fatalf("resolveSystemPrompt = %q, %v", got, err)
	}

	both := []byte(`{
		"listen": "127.0.0.1:8443",
		"mode": "hybrid",
		"llm_base_url": "http://127.0.0.1:8317/v1",
		"llm_model": "gemini-3-flash",
		"llm_system_prompt": "inline",
		"llm_system_prompt_file": "` + promptFile + `"
	}`)
	if _, err := parseConfig(both, false); err == nil {
		t.Fatalf("expected error when both llm_system_prompt and llm_system_prompt_file are set")
	}

	eng := &bridge.Engine{}
	eng.SetSystemPrompt(got)
	if err := os.WriteFile(promptFile, []byte("Reloaded prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := parseConfig(raw, false)
	if err != nil {
		t.Fatalf("second parseConfig failed: %v", err)
	}
	if err := applyReload(next, cfg, eng); err != nil {
		t.Fatalf("applyReload failed: %v", err)
	}
}
