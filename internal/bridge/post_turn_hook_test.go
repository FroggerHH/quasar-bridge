package bridge

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/dialogue"
)

type hookRequest struct {
	method, contentType string
	payload             postTurnPayload
}

// hookServer records every request; handler decides the response.
func hookServer(t *testing.T, handler func(http.ResponseWriter)) (*httptest.Server, <-chan hookRequest) {
	t.Helper()
	requests := make(chan hookRequest, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload postTurnPayload
		if err := json.UnmarshalRead(r.Body, &payload); err != nil {
			t.Errorf("hook payload: %v", err)
		}
		requests <- hookRequest{r.Method, r.Header.Get("Content-Type"), payload}
		if handler != nil {
			handler(w)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func newTestHook(t *testing.T, url string, timeoutMS int) *PostTurnHook {
	t.Helper()
	hook, err := NewPostTurnHook(PostTurnHookConfig{URL: url, TimeoutMS: timeoutMS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hook.Close)
	return hook
}

func receive(t *testing.T, requests <-chan hookRequest) hookRequest {
	t.Helper()
	select {
	case r := <-requests:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("hook was not called")
		return hookRequest{}
	}
}

func expectNone(t *testing.T, requests <-chan hookRequest) {
	t.Helper()
	select {
	case r := <-requests:
		t.Fatalf("unexpected hook call: %+v", r.payload)
	case <-time.After(150 * time.Millisecond):
	}
}

// syncBuffer lets the hook worker log while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := new(syncBuffer)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log %q not written; got %s", want, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPostTurnHookReceivesDeliveredTurn(t *testing.T) {
	server, requests := hookServer(t, nil)
	engine := &Engine{PostTurnHook: newTestHook(t, server.URL, 1000)}
	ctx := dialogue.WithSpeaker(t.Context(), "kitchen")

	engine.RecordDelivered(ctx, "llm", "Расскажи про замок", `<speaker is_whisper="true">Зам+ок sil <[500]> стоит на холме.`, true, false)

	got := receive(t, requests)
	if got.method != http.MethodPost || got.contentType != "application/json" {
		t.Fatalf("request = %s %q", got.method, got.contentType)
	}
	if got.payload.UserText != "Расскажи про замок" || got.payload.AssistantText != "Замок стоит на холме." {
		t.Fatalf("payload texts = %+v", got.payload)
	}
	if got.payload.Route != "llm" || got.payload.SpeakerID != dialogue.SpeakerKey(ctx) || got.payload.SpeakerID == "" {
		t.Fatalf("payload identity = %+v", got.payload)
	}
	if _, err := time.Parse(time.RFC3339, got.payload.Timestamp); err != nil {
		t.Fatalf("timestamp %q: %v", got.payload.Timestamp, err)
	}
}

func TestPostTurnHookFiltersTurns(t *testing.T) {
	server, requests := hookServer(t, nil)
	hook := newTestHook(t, server.URL, 1000)
	ctx := t.Context()

	hook.Notify(ctx, "home", "Включи свет", "Свет включён.", true)
	if got := receive(t, requests); got.payload.Route != "home" {
		t.Fatalf("home turn = %+v", got.payload)
	}
	for name, call := range map[string]func(){
		"cloud route":       func() { hook.Notify(ctx, "yandex", "Который час", "Три часа.", true) },
		"progress fragment": func() { hook.Notify(ctx, "llm", "Вопрос", "Думаю", false) },
		"empty answer":      func() { hook.Notify(ctx, "llm", "Вопрос", "  ", true) },
		"markup only":       func() { hook.Notify(ctx, "llm", "Вопрос", `<speaker effect="-">`, true) },
		"empty question":    func() { hook.Notify(ctx, "llm", " ", "Ответ", true) },
	} {
		call()
		t.Log("checked", name)
		expectNone(t, requests)
	}
}

func TestPostTurnHookSkipsAnnouncements(t *testing.T) {
	server, requests := hookServer(t, nil)
	engine := &Engine{PostTurnHook: newTestHook(t, server.URL, 1000)}

	engine.RecordAnnouncement(dialogue.WithSpeaker(t.Context(), "kitchen"), "Чайник вскипел.")

	expectNone(t, requests)
}

func TestPostTurnHookDoesNotBlockOnSlowEndpoint(t *testing.T) {
	release := make(chan struct{})
	server, requests := hookServer(t, func(http.ResponseWriter) { <-release })
	defer close(release)
	hook := newTestHook(t, server.URL, 30000)
	engine := &Engine{PostTurnHook: hook}
	ctx := dialogue.WithSpeaker(t.Context(), "kitchen")

	start := time.Now()
	for range postTurnHookQueueSize * 3 {
		engine.RecordDelivered(ctx, "llm", "Вопрос", "Ответ", true, false)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("RecordDelivered blocked for %v on a hanging hook", elapsed)
	}
	receive(t, requests) // the worker is stuck in the first request

	start = time.Now()
	hook.Close() // aborts the hanging request instead of waiting for the 30 s timeout
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
}

func TestPostTurnHookDropsEventsWhenQueueIsFull(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	server, requests := hookServer(t, func(http.ResponseWriter) { <-release })
	defer close(release)
	hook := newTestHook(t, server.URL, 30000)

	hook.Notify(t.Context(), "llm", "first", "answer", true)
	receive(t, requests) // taken by the worker, which now hangs
	for range postTurnHookQueueSize + 5 {
		hook.Notify(t.Context(), "llm", "queued", "answer", true)
	}

	if len(hook.events) != postTurnHookQueueSize {
		t.Fatalf("queue length = %d, want %d", len(hook.events), postTurnHookQueueSize)
	}
	if !strings.Contains(logs.String(), "post_turn_hook_dropped") {
		t.Fatalf("overflow not logged: %s", logs.String())
	}
}

func TestPostTurnHookLogsFailuresWithoutLeakingText(t *testing.T) {
	const question, answer = "секретный вопрос", "секретный ответ"
	for name, setup := range map[string]func(t *testing.T) string{
		"server error": func(t *testing.T) string {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, question+answer, http.StatusInternalServerError)
			}))
			t.Cleanup(server.Close)
			return server.URL
		},
		"redirect": func(t *testing.T) string {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			return server.URL
		},
		"unreachable": func(t *testing.T) string {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return server.URL + "/token-in-path"
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			hook := newTestHook(t, setup(t), 1000)

			hook.Notify(t.Context(), "llm", question, answer, true)
			waitForLog(t, logs, "post_turn_hook_failed")
			hook.Notify(t.Context(), "llm", question, answer, true) // keeps working after a failure
			hook.Close()

			if out := logs.String(); strings.Contains(out, question) || strings.Contains(out, answer) || strings.Contains(out, "token-in-path") {
				t.Fatalf("failure log leaks content: %s", out)
			}
		})
	}
}

func TestPostTurnHookTimesOutSlowEndpoint(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	server, _ := hookServer(t, func(http.ResponseWriter) { <-release })
	defer close(release)
	hook := newTestHook(t, server.URL, 200)

	hook.Notify(t.Context(), "llm", "Вопрос", "Ответ", true)

	waitForLog(t, logs, "post_turn_hook_failed")
}

func TestPostTurnHookCloseIsIdempotentAndStopsDelivery(t *testing.T) {
	server, requests := hookServer(t, nil)
	hook := newTestHook(t, server.URL, 1000)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				hook.Notify(t.Context(), "llm", "Вопрос", "Ответ", true)
			}
		})
	}
	hook.Close()
	hook.Close()
	wg.Wait()
	for len(requests) > 0 {
		<-requests
	}

	hook.Notify(t.Context(), "llm", "Вопрос", "Ответ", true)
	expectNone(t, requests)
}

func TestPostTurnHookNilAndDisabledAreNoops(t *testing.T) {
	var hook *PostTurnHook
	hook.Notify(t.Context(), "llm", "Вопрос", "Ответ", true)
	hook.Close()
	(&Engine{}).RecordDelivered(t.Context(), "llm", "Вопрос", "Ответ", true, false)

	got, err := NewPostTurnHook(PostTurnHookConfig{})
	if got != nil || err != nil {
		t.Fatalf("disabled config = %v, %v", got, err)
	}
}

func TestPostTurnHookConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  PostTurnHookConfig
		want string
	}{
		{name: "disabled", cfg: PostTurnHookConfig{}},
		{name: "disabled ignores timeout", cfg: PostTurnHookConfig{TimeoutMS: 1}},
		{name: "http", cfg: PostTurnHookConfig{URL: "http://127.0.0.1:9000/turn", TimeoutMS: 5000}},
		{name: "https", cfg: PostTurnHookConfig{URL: "https://memory.local/turn", TimeoutMS: 200}},
		{name: "upper timeout", cfg: PostTurnHookConfig{URL: "http://127.0.0.1:9000", TimeoutMS: 30000}},
		{name: "ftp", cfg: PostTurnHookConfig{URL: "ftp://host/turn", TimeoutMS: 5000}, want: "post_turn_hook.url"},
		{name: "no scheme", cfg: PostTurnHookConfig{URL: "127.0.0.1:9000", TimeoutMS: 5000}, want: "post_turn_hook.url"},
		{name: "no host", cfg: PostTurnHookConfig{URL: "http:///turn", TimeoutMS: 5000}, want: "post_turn_hook.url"},
		{name: "credentials", cfg: PostTurnHookConfig{URL: "http://user:pass@host/turn", TimeoutMS: 5000}, want: "post_turn_hook.url"},
		{name: "timeout too small", cfg: PostTurnHookConfig{URL: "http://host", TimeoutMS: 199}, want: "timeout_ms"},
		{name: "timeout too large", cfg: PostTurnHookConfig{URL: "http://host", TimeoutMS: 30001}, want: "timeout_ms"},
		{name: "timeout zero", cfg: PostTurnHookConfig{URL: "http://host"}, want: "timeout_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStripTTSMarkup(t *testing.T) {
	for in, want := range map[string]string{
		"Обычный ответ.":                                "Обычный ответ.",
		`<speaker voice="zahar">Привет sil <[300]> мир`: "Привет мир",
		"Зам+ок и 2+2":                                  "Замок и 2+2",
	} {
		if got := StripTTSMarkup(in); got != want {
			t.Errorf("StripTTSMarkup(%q) = %q, want %q", in, got, want)
		}
	}
}
