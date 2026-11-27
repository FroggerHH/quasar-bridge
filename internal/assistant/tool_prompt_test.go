package assistant

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

// stubToolset offers fixed tool names and optionally reports a tool status.
type stubToolset struct {
	names  []string
	status string
}

func (s stubToolset) Definitions() []llm.Tool {
	tools := make([]llm.Tool, 0, len(s.names))
	for _, name := range s.names {
		tools = append(tools, llm.Tool{Type: "function", Function: llm.Function{Name: name}})
	}
	return tools
}
func (s stubToolset) Call(context.Context, string, string) (any, error) { return nil, nil }
func (s stubToolset) ToolStatus() string                                { return s.status }

// systemPromptSeen runs one agent turn and returns the system message and the
// number of system messages the model received.
func systemPromptSeen(t *testing.T, tools Toolset, messages []llm.Message) (string, int) {
	t.Helper()
	var system string
	var systems int
	agent := Agent{Tools: tools, MaxRounds: 1, MaxCalls: 1, Client: chatFunc(func(_ context.Context, m []llm.Message, _ []llm.Tool) (llm.Message, error) {
		for _, message := range m {
			if message.Role == "system" {
				systems++
				system = message.Content
			}
		}
		return llm.Message{Content: "ok"}, nil
	})}
	if _, err := agent.Complete(t.Context(), messages, false); err != nil {
		t.Fatal(err)
	}
	return system, systems
}

func TestToolPromptOmitsDisabledFeatures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Time, cfg.History, cfg.Web = true, true, true
	cfg.Configuration, cfg.PlaySound, cfg.Updates = false, false, false
	// No Configuration, AirPlay, Playback or Updater: those tools are not offered.
	system, systems := systemPromptSeen(t, &Tools{Config: cfg}, []llm.Message{{Role: "user", Content: "привет"}})
	if systems != 1 {
		t.Fatalf("want one system message, got %d", systems)
	}
	for _, leaked := range []string{"AirPlay", "airplay", "play_music", "play_sound", "get_quasar_bridge_config", "routing", "saved_revision"} {
		if strings.Contains(system, leaked) {
			t.Errorf("disabled feature leaked %q into the prompt:\n%s", leaked, system)
		}
	}
	for _, wanted := range []string{"недоверенные данные", "search_history", "current_time"} {
		if !strings.Contains(system, wanted) {
			t.Errorf("prompt lacks %q:\n%s", wanted, system)
		}
	}
}

func TestToolPromptIncludesEnabledFeatures(t *testing.T) {
	cases := []struct {
		name   string
		tools  []string
		wanted []string
		absent []string
	}{
		{"configuration only", []string{"get_quasar_bridge_config"}, []string{"get_quasar_bridge_config"}, []string{"saved_revision", "AirPlay"}},
		{"routing only", []string{"get_quasar_bridge_routing"}, []string{"saved_revision"}, []string{"get_quasar_bridge_config", "AirPlay"}},
		{"airplay", []string{"get_airplay_status", "control_airplay"}, []string{"get_airplay_status", "connected"}, []string{"play_music", "saved_revision"}},
		{"music", []string{"play_music"}, []string{"play_music", "MP3"}, []string{"AirPlay"}},
		{"sound", []string{"play_sound"}, []string{"play_sound", "queued"}, []string{"play_music"}},
		{"history", []string{"search_history"}, []string{"search_history", "часовому поясу"}, []string{"current_time"}},
		{"time", []string{"current_time"}, []string{"current_time"}, []string{"search_history"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system, _ := systemPromptSeen(t, stubToolset{names: tc.tools}, nil)
			for _, wanted := range tc.wanted {
				if !strings.Contains(system, wanted) {
					t.Errorf("missing %q in:\n%s", wanted, system)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(system, absent) {
					t.Errorf("unexpected %q in:\n%s", absent, system)
				}
			}
		})
	}
}

func TestToolPromptIsConciseWithoutOptionalTools(t *testing.T) {
	if prompt := buildToolPrompt(nil); prompt != coreToolPrompt {
		t.Fatalf("no tools should yield only the core rules, got:\n%s", prompt)
	}
	if got := utf8.RuneCountInString(coreToolPrompt); got > 800 {
		t.Fatalf("core tool rules grew to %d characters", got)
	}
}

func TestIncomingSystemMessagePrecedesToolInstructions(t *testing.T) {
	tools := stubToolset{names: []string{"current_time"}, status: "Недоступны MCP-серверы: demo."}
	system, systems := systemPromptSeen(t, tools, []llm.Message{
		{Role: "system", Content: "PERSONA: отвечай как пират"},
		{Role: "user", Content: "время"},
		{Role: "system", Content: "FORMAT: одно предложение"},
	})
	if systems != 1 {
		t.Fatalf("want one merged system message, got %d", systems)
	}
	persona := strings.Index(system, "PERSONA")
	format := strings.Index(system, "FORMAT")
	core := strings.Index(system, coreToolPrompt)
	timeRule := strings.Index(system, timeToolPrompt)
	status := strings.Index(system, "Недоступны MCP-серверы")
	if persona != 0 || !(persona < format && format < core && core < timeRule && timeRule < status) {
		t.Fatalf("unexpected order persona=%d format=%d core=%d time=%d status=%d:\n%s", persona, format, core, timeRule, status, system)
	}
}

func TestLimitNoticeIsAppendedToMergedSystemMessage(t *testing.T) {
	var last string
	agent := Agent{Tools: stubToolset{names: []string{"current_time"}}, MaxRounds: 0, MaxCalls: 1, Client: chatFunc(func(_ context.Context, m []llm.Message, defs []llm.Tool) (llm.Message, error) {
		last = m[0].Content
		if len(defs) != 0 {
			t.Error("tools must be withdrawn once the round limit is reached")
		}
		return llm.Message{Content: "ok"}, nil
	})}
	if _, err := agent.Complete(t.Context(), []llm.Message{{Role: "system", Content: "PERSONA"}}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(last, "PERSONA") || !strings.HasSuffix(last, "если данных недостаточно, скажи об этом.") {
		t.Fatalf("limit notice missing or persona displaced:\n%s", last)
	}
}
