package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

// modelRecorder records the model carried by each call's context.
type modelRecorder struct {
	mu     sync.Mutex
	models []string
	calls  int
	reply  func(ctx context.Context, structured bool) (string, error)
}

func (m *modelRecorder) Complete(ctx context.Context, _ []llm.Message, structured bool) (string, error) {
	m.mu.Lock()
	m.calls++
	m.models = append(m.models, llm.ModelFromContext(ctx))
	m.mu.Unlock()
	return m.reply(ctx, structured)
}

func (m *modelRecorder) snapshot() (calls int, models []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, append([]string(nil), m.models...)
}

func enabledRouting() ModelRoutingConfig {
	cfg := DefaultModelRoutingConfig()
	cfg.Enabled = true
	cfg.SmartModel = "smart-model"
	return cfg
}

func triageAnswering(answer string, err error) *modelRecorder {
	return &modelRecorder{reply: func(context.Context, bool) (string, error) { return answer, err }}
}

func TestModelRoutingConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ModelRoutingConfig)
		ok     bool
	}{
		{"disabled ignores everything", func(c *ModelRoutingConfig) { c.Enabled = false; c.SmartModel = ""; c.TriageTimeoutMS = -5 }, true},
		{"enabled", func(*ModelRoutingConfig) {}, true},
		{"missing smart model", func(c *ModelRoutingConfig) { c.SmartModel = " " }, false},
		{"zero timeout means default", func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 0 }, true},
		{"timeout lower bound", func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 200 }, true},
		{"timeout upper bound", func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 15000 }, true},
		{"timeout too small", func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 199 }, false},
		{"timeout too large", func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 15001 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabledRouting()
			tc.mutate(&cfg)
			if err := cfg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%t", err, tc.ok)
			}
		})
	}
	if got := DefaultModelRoutingConfig(); got.Enabled || got.TriageTimeoutMS != 2500 {
		t.Fatalf("default = %+v", got)
	}
}

func TestModelRouterSelect(t *testing.T) {
	const fast, smart, triageModel = "fast-model", "smart-model", "triage-model"
	for _, tc := range []struct {
		name         string
		cfg          func(*ModelRoutingConfig)
		text         string
		triage       *modelRecorder
		wantModel    string
		wantTier     string
		wantSource   string
		wantTriage   int
		wantTriageAs string
	}{
		{name: "disabled", cfg: func(c *ModelRoutingConfig) { c.Enabled = false }, text: "сравни два телефона подробно", wantModel: fast, wantTier: "fast", wantSource: "disabled"},
		{name: "heuristic smart russian", text: "Сравни iPhone и Pixel по камере", wantModel: smart, wantTier: "smart", wantSource: "heuristic"},
		{name: "heuristic smart short with cue", text: "разбери это", wantModel: smart, wantTier: "smart", wantSource: "heuristic"},
		{name: "heuristic smart background", text: "запусти в фоне проверку", wantModel: smart, wantTier: "smart", wantSource: "heuristic"},
		{name: "heuristic smart english", text: "Please compare these two laptops", wantModel: smart, wantTier: "smart", wantSource: "heuristic"},
		{name: "heuristic fast greeting", text: "Привет", wantModel: fast, wantTier: "fast", wantSource: "heuristic"},
		{name: "heuristic fast three words", text: "который сейчас час", wantModel: fast, wantTier: "fast", wantSource: "heuristic"},
		{name: "triage smart", text: "как лучше организовать переезд в другую страну", triage: triageAnswering(`{"tier":"smart"}`, nil), wantModel: smart, wantTier: "smart", wantSource: "triage", wantTriage: 1, wantTriageAs: triageModel},
		{name: "triage fast", text: "сколько стоит билет в метро сегодня", triage: triageAnswering(`{"tier":"fast"}`, nil), wantModel: fast, wantTier: "fast", wantSource: "triage", wantTriage: 1, wantTriageAs: triageModel},
		{name: "triage error", text: "сколько стоит билет в метро сегодня", triage: triageAnswering("", errors.New("boom")), wantModel: fast, wantTier: "fast", wantSource: "fallback", wantTriage: 1, wantTriageAs: triageModel},
		{name: "triage invalid json", text: "сколько стоит билет в метро сегодня", triage: triageAnswering("smart", nil), wantModel: fast, wantTier: "fast", wantSource: "fallback", wantTriage: 1, wantTriageAs: triageModel},
		{name: "triage unknown tier", text: "сколько стоит билет в метро сегодня", triage: triageAnswering(`{"tier":"huge"}`, nil), wantModel: fast, wantTier: "fast", wantSource: "fallback", wantTriage: 1, wantTriageAs: triageModel},
		{name: "triage defaults to fast model", cfg: func(c *ModelRoutingConfig) { c.TriageModel = "" }, text: "сколько стоит билет в метро сегодня", triage: triageAnswering(`{"tier":"fast"}`, nil), wantModel: fast, wantTier: "fast", wantSource: "triage", wantTriage: 1, wantTriageAs: fast},
		{name: "triage timeout", cfg: func(c *ModelRoutingConfig) { c.TriageTimeoutMS = 200 }, text: "сколько стоит билет в метро сегодня", triage: &modelRecorder{reply: func(ctx context.Context, _ bool) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(5 * time.Second):
				return `{"tier":"smart"}`, nil
			}
		}}, wantModel: fast, wantTier: "fast", wantSource: "fallback", wantTriage: 1, wantTriageAs: triageModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabledRouting()
			cfg.TriageModel = triageModel
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			var completer Completer
			if tc.triage != nil {
				completer = tc.triage
			} else {
				completer = triageAnswering("", errors.New("triage must not run"))
			}
			router, err := NewModelRouter(cfg, fast, completer)
			if err != nil {
				t.Fatal(err)
			}
			model, tier, source := router.Select(t.Context(), tc.text)
			if model != tc.wantModel || tier != tc.wantTier || source != tc.wantSource {
				t.Fatalf("Select = (%q, %q, %q), want (%q, %q, %q)", model, tier, source, tc.wantModel, tc.wantTier, tc.wantSource)
			}
			if tc.triage == nil {
				if calls, _ := completer.(*modelRecorder).snapshot(); calls != 0 {
					t.Fatalf("unexpected triage calls: %d", calls)
				}
				return
			}
			calls, models := tc.triage.snapshot()
			if calls != tc.wantTriage {
				t.Fatalf("triage calls = %d, want %d", calls, tc.wantTriage)
			}
			if calls > 0 && models[0] != tc.wantTriageAs {
				t.Fatalf("triage ran with model %q, want %q", models[0], tc.wantTriageAs)
			}
		})
	}
}

func TestNewModelRouter(t *testing.T) {
	cfg := enabledRouting()
	cfg.SmartModel = ""
	if _, err := NewModelRouter(cfg, "fast", nil); err == nil {
		t.Fatal("missing smart model accepted")
	}
	// Without a triage backend ambiguous requests safely stay on the fast model.
	router, err := NewModelRouter(enabledRouting(), "fast", nil)
	if err != nil {
		t.Fatal(err)
	}
	if model, tier, source := router.Select(t.Context(), "сколько стоит билет в метро сегодня"); model != "fast" || tier != "fast" || source != "fallback" {
		t.Fatalf("Select without triage backend = (%q, %q, %q)", model, tier, source)
	}
}

func TestFallbackPassesSelectedModelToLLMAndAssistant(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		assistant        bool
	}{
		{"llm smart", "сравни два тарифа подробно", "smart-model", false},
		{"llm fast", "привет", "fast-model", false},
		{"assistant smart", "сравни два тарифа подробно", "smart-model", true},
		{"assistant fast", "привет", "fast-model", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answerer := &modelRecorder{reply: func(context.Context, bool) (string, error) { return "Ответ", nil }}
			engine := &Engine{LLM: answerer}
			if tc.assistant {
				assistantCompleter := &modelRecorder{reply: func(context.Context, bool) (string, error) { return "Ответ", nil }}
				engine.Assistant = assistantCompleter
				answerer = assistantCompleter
			}
			if err := engine.SetModelRouting(enabledRouting(), "fast-model"); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Fallback(t.Context(), tc.text); err != nil {
				t.Fatal(err)
			}
			calls, models := answerer.snapshot()
			if calls != 1 || models[0] != tc.want {
				t.Fatalf("calls=%d models=%v, want one call with %q", calls, models, tc.want)
			}
			if tc.assistant {
				if calls, _ := engine.LLM.(*modelRecorder).snapshot(); calls != 0 {
					t.Fatalf("LLM used instead of assistant: %d calls", calls)
				}
			}
		})
	}
}

func TestFallbackTriagesAmbiguousRequestOnLLM(t *testing.T) {
	llmCompleter := &modelRecorder{reply: func(_ context.Context, structured bool) (string, error) {
		if structured {
			return `{"tier":"smart"}`, nil
		}
		return "Ответ", nil
	}}
	engine := &Engine{LLM: llmCompleter}
	if err := engine.SetModelRouting(enabledRouting(), "fast-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Fallback(t.Context(), "как лучше организовать переезд в другую страну"); err != nil {
		t.Fatal(err)
	}
	calls, models := llmCompleter.snapshot()
	if calls != 2 || models[0] != "fast-model" || models[1] != "smart-model" {
		t.Fatalf("calls=%d models=%v, want triage on fast-model then answer on smart-model", calls, models)
	}
}

func TestFallbackSurvivesTriageFailure(t *testing.T) {
	llmCompleter := &modelRecorder{reply: func(_ context.Context, structured bool) (string, error) {
		if structured {
			return "", errors.New("triage down")
		}
		return "Ответ", nil
	}}
	engine := &Engine{LLM: llmCompleter}
	if err := engine.SetModelRouting(enabledRouting(), "fast-model"); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Fallback(t.Context(), "как лучше организовать переезд в другую страну")
	if err != nil || got.Answer != "Ответ" {
		t.Fatalf("Fallback = %+v, %v", got, err)
	}
	if _, models := llmCompleter.snapshot(); models[len(models)-1] != "fast-model" {
		t.Fatalf("answer model = %v, want fast-model", models)
	}
}

func TestRetryUsesModelSelectedForReplayedQuestion(t *testing.T) {
	var guardModels, answerModels []string
	engine := &Engine{
		Dialogue: dialogue.New(dialogue.DefaultConfig()),
		LLM: completionFunc(func(ctx context.Context, _ []llm.Message, structured bool) (string, error) {
			if structured {
				guardModels = append(guardModels, llm.ModelFromContext(ctx))
				return `{}`, nil
			}
			answerModels = append(answerModels, llm.ModelFromContext(ctx))
			return "Ответ.", nil
		}),
	}
	if err := engine.SetModelRouting(enabledRouting(), "fast-model"); err != nil {
		t.Fatal(err)
	}
	ctx := dialogue.WithSpeaker(t.Context(), "a")
	_, first := engine.Dialogue.Begin(ctx)
	engine.Dialogue.CaptureReplay(ctx, first, "сравни два тарифа подробно", "Не могу ответить.", true, false, false, false)
	retryCtx, _, ticket := engine.Dialogue.Start(ctx)
	if _, err := engine.retryQuestion(retryCtx, ticket); err != nil {
		t.Fatal(err)
	}
	if len(answerModels) != 1 || answerModels[0] != "smart-model" {
		t.Fatalf("retry answer models = %v, want smart-model", answerModels)
	}
	for _, model := range guardModels {
		if model != "" {
			t.Fatalf("safety guard ran with routed model %q", model)
		}
	}
}

func TestDisabledModelRoutingLeavesContextUntouched(t *testing.T) {
	for _, configure := range []func(*Engine) error{
		func(*Engine) error { return nil },
		func(e *Engine) error { return e.SetModelRouting(ModelRoutingConfig{}, "fast-model") },
		func(e *Engine) error {
			if err := e.SetModelRouting(enabledRouting(), "fast-model"); err != nil {
				return err
			}
			return e.SetModelRouting(ModelRoutingConfig{}, "fast-model")
		},
	} {
		answerer := &modelRecorder{reply: func(context.Context, bool) (string, error) { return "Ответ", nil }}
		engine := &Engine{LLM: answerer}
		if err := configure(engine); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Fallback(t.Context(), "сравни два тарифа подробно, а также расскажи про третий"); err != nil {
			t.Fatal(err)
		}
		if calls, models := answerer.snapshot(); calls != 1 || models[0] != "" {
			t.Fatalf("disabled routing changed requests: calls=%d models=%v", calls, models)
		}
	}
}

func TestSetModelRoutingRejectsInvalidConfig(t *testing.T) {
	engine := &Engine{}
	cfg := enabledRouting()
	cfg.SmartModel = ""
	if err := engine.SetModelRouting(cfg, "fast-model"); err == nil {
		t.Fatal("enabled routing without smart_model accepted")
	}
}
