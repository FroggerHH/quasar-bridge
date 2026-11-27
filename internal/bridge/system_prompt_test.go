package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type captureCompleter struct {
	got    []llm.Message
	answer string
}

func (c *captureCompleter) Complete(_ context.Context, msgs []llm.Message, _ bool) (string, error) {
	c.got = append([]llm.Message(nil), msgs...)
	return c.answer, nil
}

func TestReloadedSystemPromptKeepsTTSInstructionsWithoutBuiltinPersona(t *testing.T) {
	stub := &captureCompleter{
		answer: `<speaker is_whisper="true">Секрет. sil <[400]> <speaker is_whisper="false">Зам+ок закрыт.`,
	}
	eng := &Engine{
		LLM:       stub,
		TTSMarkup: true,
	}
	const custom = "CUSTOM EN PROMPT ONLY"
	eng.SetSystemPrompt(custom)

	res, err := eng.Fallback(t.Context(), "ты подслушиваешь")
	if err != nil {
		t.Fatalf("Fallback error: %v", err)
	}
	if len(stub.got) == 0 || stub.got[0].Role != "system" {
		t.Fatalf("expected system message first, got %#v", stub.got)
	}
	if stub.got[0].Content != custom+eng.ttsInstructions() {
		t.Fatalf("system prompt mismatch: got %q, want %q", stub.got[0].Content, custom)
	}
	if strings.Contains(stub.got[0].Content, defaultSystemPrompt) {
		t.Fatalf("builtin persona must not be appended when custom prompt is set")
	}
	if res.speech != stub.answer {
		t.Fatalf("expected tagged speech to be preserved: got %q, want %q", res.speech, stub.answer)
	}
	if res.Answer != "Секрет. Замок закрыт." {
		t.Fatalf("expected stripped plain answer: got %q", res.Answer)
	}

	eng.SetSystemPrompt("UPDATED PROMPT")
	if _, err := eng.Fallback(t.Context(), "ещё вопрос"); err != nil {
		t.Fatalf("second Fallback error: %v", err)
	}
	if stub.got[0].Content != "UPDATED PROMPT"+eng.ttsInstructions() {
		t.Fatalf("hot-reloaded prompt not applied: got %q", stub.got[0].Content)
	}
}
