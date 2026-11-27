package main

import (
	"context"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func routingConfigJSON(routing string) []byte {
	return []byte(`{
		"listen": "127.0.0.1:8443",
		"mode": "hybrid",
		"llm_base_url": "http://127.0.0.1:8317/v1",
		"llm_model": "gemini-3-flash"` + routing + `
	}`)
}

func TestLLMModelRoutingConfig(t *testing.T) {
	for _, tc := range []struct {
		name, routing string
		wantErr       string
		want          bridge.ModelRoutingConfig
	}{
		{name: "absent is disabled with default timeout", want: bridge.ModelRoutingConfig{TriageTimeoutMS: 2500}},
		{name: "disabled without smart model", routing: `,"llm_model_routing":{"enabled":false}`, want: bridge.ModelRoutingConfig{TriageTimeoutMS: 2500}},
		{
			name:    "enabled",
			routing: `,"llm_model_routing":{"enabled":true,"smart_model":"gemini-3.8-flash-high","triage_model":"voice-fast","triage_timeout_ms":1000}`,
			want:    bridge.ModelRoutingConfig{Enabled: true, SmartModel: "gemini-3.8-flash-high", TriageModel: "voice-fast", TriageTimeoutMS: 1000},
		},
		{
			name:    "enabled keeps default timeout",
			routing: `,"llm_model_routing":{"enabled":true,"smart_model":"voice-smart"}`,
			want:    bridge.ModelRoutingConfig{Enabled: true, SmartModel: "voice-smart", TriageTimeoutMS: 2500},
		},
		{name: "enabled needs smart model", routing: `,"llm_model_routing":{"enabled":true}`, wantErr: "smart_model"},
		{name: "timeout too small", routing: `,"llm_model_routing":{"enabled":true,"smart_model":"s","triage_timeout_ms":100}`, wantErr: "triage_timeout_ms"},
		{name: "timeout too large", routing: `,"llm_model_routing":{"enabled":true,"smart_model":"s","triage_timeout_ms":20000}`, wantErr: "triage_timeout_ms"},
		{name: "unknown member", routing: `,"llm_model_routing":{"enabled":true,"smart_model":"s","fast_model":"f"}`, wantErr: "invalid configuration JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(routingConfigJSON(tc.routing), false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseConfig error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLMModelRouting != tc.want {
				t.Fatalf("LLMModelRouting = %+v, want %+v", cfg.LLMModelRouting, tc.want)
			}
		})
	}
}

type modelCapture struct{ models []string }

func (m *modelCapture) Complete(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
	m.models = append(m.models, llm.ModelFromContext(ctx))
	return "Ответ", nil
}

func TestModelRoutingReloadsWithoutRestart(t *testing.T) {
	initial, err := parseConfig(routingConfigJSON(""), false)
	if err != nil {
		t.Fatal(err)
	}
	capture := &modelCapture{}
	engine := &bridge.Engine{LLM: capture}
	if err := engine.SetModelRouting(initial.LLMModelRouting, initial.LLMModel); err != nil {
		t.Fatal(err)
	}
	ask := func() string {
		capture.models = nil
		if _, err := engine.Fallback(t.Context(), "сравни два тарифа подробно"); err != nil {
			t.Fatal(err)
		}
		return capture.models[len(capture.models)-1]
	}
	if got := ask(); got != "" {
		t.Fatalf("routing disabled but model override %q was sent", got)
	}

	enabled, err := parseConfig(routingConfigJSON(`,"llm_model_routing":{"enabled":true,"smart_model":"voice-smart"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyReload(enabled, initial, engine); err != nil {
		t.Fatalf("enabling routing needs no restart: %v", err)
	}
	if got := ask(); got != "voice-smart" {
		t.Fatalf("after reload model = %q, want voice-smart", got)
	}

	if err := applyReload(initial, enabled, engine); err != nil {
		t.Fatal(err)
	}
	if got := ask(); got != "" {
		t.Fatalf("after disabling reload model = %q, want default", got)
	}
}

func TestReloadRejectsInvalidModelRoutingAndModelChange(t *testing.T) {
	initial, err := parseConfig(routingConfigJSON(""), false)
	if err != nil {
		t.Fatal(err)
	}
	engine := &bridge.Engine{LLM: &modelCapture{}}

	invalid := initial
	invalid.LLMModelRouting = bridge.ModelRoutingConfig{Enabled: true}
	if err := applyReload(invalid, initial, engine); err == nil {
		t.Fatal("reload accepted enabled routing without smart_model")
	}

	changed := initial
	changed.LLMModel = "another-model"
	if err := applyReload(changed, initial, engine); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("llm_model change error = %v, want restart required", err)
	}
}
