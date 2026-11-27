package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func TestPanelReloadPreservesHarnessExtensions(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(prompt, []byte("First persona"), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"diagnostics":            map[string]any{"network_access": true, "port": 2704},
		"llm_system_prompt_file": prompt,
		"llm_model":              "fast",
		"llm_model_routing":      map[string]any{"enabled": true, "smart_model": "smart"},
		"post_turn_hook":         map[string]any{"url": "http://127.0.0.1:8765/hooks/turn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := configurationFixture(t, string(body))
	var gotPrompt, gotModel string
	c.engine.LLM = diagnosticCompleter(func(ctx context.Context, messages []llm.Message, _ bool) (string, error) {
		gotPrompt, gotModel = messages[0].Content, llm.ModelFromContext(ctx)
		return "Fixture answer", nil
	})
	if err := os.WriteFile(prompt, []byte("Reloaded persona"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Configuration: c})
	w := diagnosticAPIRequest(mux, "POST", "apply", `{"action":"reload"}`, "application/json")
	if w.Code != 200 {
		t.Fatalf("reload status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := c.engine.Fallback(t.Context(), "сравни варианты"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPrompt, "Reloaded persona") || gotModel != "smart" {
		t.Fatalf("prompt=%q model=%q", gotPrompt, gotModel)
	}
	if !c.active.Diagnostics.NetworkAccess || c.active.Diagnostics.Port != 2704 || c.active.PostTurnHook.URL != "http://127.0.0.1:8765/hooks/turn" {
		t.Fatal("panel reload lost harness or diagnostics configuration")
	}
	if result := c.result("fixture", c.active); result.RestartRequired {
		t.Fatal("applied configuration reports a pending restart")
	}

	// A listener failure rolls back the prompt and model router as well.
	next := c.active
	next.Diagnostics.NetworkAccess = false
	next.LLMSystemPromptFile = ""
	next.LLMSystemPrompt = "Rejected persona"
	next.LLMModelRouting.SmartModel = "rejected-smart"
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c.applyDiagnostics = func(bool) error { return context.DeadlineExceeded }
	if _, err := c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("listener failure accepted")
	}
	if _, err := c.engine.Fallback(t.Context(), "сравни варианты"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPrompt, "Reloaded persona") || gotModel != "smart" {
		t.Fatalf("rollback lost prompt/router: %q %q", gotPrompt, gotModel)
	}

	next = c.active
	next.PostTurnHook = bridge.PostTurnHookConfig{}
	if !requiresRestart(c.active, next) {
		t.Fatal("hook changes must still require restart")
	}
}
