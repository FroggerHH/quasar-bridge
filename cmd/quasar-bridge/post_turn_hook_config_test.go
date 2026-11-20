package main

import (
	"os"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

func TestPostTurnHookConfig(t *testing.T) {
	for _, tc := range []struct {
		name, hook string
		wantErr    string
		want       bridge.PostTurnHookConfig
	}{
		{name: "absent is disabled with default timeout", want: bridge.PostTurnHookConfig{TimeoutMS: 5000}},
		{name: "empty url is disabled", hook: `,"post_turn_hook":{"url":""}`, want: bridge.PostTurnHookConfig{TimeoutMS: 5000}},
		{
			name: "enabled keeps default timeout",
			hook: `,"post_turn_hook":{"url":"http://127.0.0.1:9000/turn"}`,
			want: bridge.PostTurnHookConfig{URL: "http://127.0.0.1:9000/turn", TimeoutMS: 5000},
		},
		{
			name: "https with timeout",
			hook: `,"post_turn_hook":{"url":"https://memory.local/turn","timeout_ms":250}`,
			want: bridge.PostTurnHookConfig{URL: "https://memory.local/turn", TimeoutMS: 250},
		},
		{name: "upper timeout", hook: `,"post_turn_hook":{"url":"http://127.0.0.1:9000","timeout_ms":30000}`, want: bridge.PostTurnHookConfig{URL: "http://127.0.0.1:9000", TimeoutMS: 30000}},
		{name: "ftp url", hook: `,"post_turn_hook":{"url":"ftp://127.0.0.1/turn"}`, wantErr: "post_turn_hook.url"},
		{name: "missing scheme", hook: `,"post_turn_hook":{"url":"127.0.0.1:9000/turn"}`, wantErr: "post_turn_hook.url"},
		{name: "credentials in url", hook: `,"post_turn_hook":{"url":"http://u:p@127.0.0.1:9000"}`, wantErr: "post_turn_hook.url"},
		{name: "timeout too small", hook: `,"post_turn_hook":{"url":"http://127.0.0.1:9000","timeout_ms":199}`, wantErr: "timeout_ms"},
		{name: "timeout too large", hook: `,"post_turn_hook":{"url":"http://127.0.0.1:9000","timeout_ms":30001}`, wantErr: "timeout_ms"},
		{name: "unknown member", hook: `,"post_turn_hook":{"url":"http://127.0.0.1:9000","retries":3}`, wantErr: "invalid configuration JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseConfig(routingConfigJSON(tc.hook), false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseConfig error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PostTurnHook != tc.want {
				t.Fatalf("PostTurnHook = %+v, want %+v", cfg.PostTurnHook, tc.want)
			}
		})
	}
}

func TestReloadPostTurnHookRequiresRestart(t *testing.T) {
	path := configFile(t, `{"post_turn_hook":{"url":"http://127.0.0.1:9000/turn"}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	revision := engine.RoutingStatus().Revision
	for _, hook := range []string{
		`{"post_turn_hook":{"url":"http://127.0.0.1:9001/turn"}}`,
		`{"post_turn_hook":{"url":"http://127.0.0.1:9000/turn","timeout_ms":1000}}`,
		`{}`,
	} {
		body := strings.TrimSuffix(hook, "}") + `,"routing":{"rules":[],"default_route":"llm"}}`
		if strings.HasPrefix(hook, "{}") {
			body = `{"routing":{"rules":[],"default_route":"llm"}}`
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := reloadRouting(path, initial, engine); err == nil || !strings.Contains(err.Error(), "restart required") {
			t.Fatalf("changed post_turn_hook accepted: %s: %v", hook, err)
		}
		if engine.RoutingStatus().Revision != revision {
			t.Fatal("rejected post_turn_hook change altered routing")
		}
	}
	unchanged := `{"post_turn_hook":{"url":"http://127.0.0.1:9000/turn"},"routing":{"rules":[],"default_route":"llm"}}`
	if err := os.WriteFile(path, []byte(unchanged), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine); err != nil {
		t.Fatalf("reload with unchanged post_turn_hook rejected: %v", err)
	}
}
