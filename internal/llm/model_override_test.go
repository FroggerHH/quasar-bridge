package llm

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

const chatAnswer = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`

func TestModelFromContext(t *testing.T) {
	if got := ModelFromContext(context.Background()); got != "" {
		t.Fatalf("empty context model = %q", got)
	}
	ctx := WithModel(context.Background(), "smart")
	if got := ModelFromContext(ctx); got != "smart" {
		t.Fatalf("model = %q", got)
	}
	if got := ModelFromContext(WithModel(ctx, "")); got != "smart" {
		t.Fatalf("empty override replaced the parent model: %q", got)
	}
	if got := ModelFromContext(WithModel(ctx, "other")); got != "other" {
		t.Fatalf("nested override = %q", got)
	}
}

func TestContextModelOverridesClientModel(t *testing.T) {
	for _, tc := range []struct{ api, answer string }{{"chat_completions", chatAnswer}, {"responses", responseAnswer}} {
		t.Run(tc.api, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.UnmarshalRead(r.Body, &body); err != nil {
					t.Error(err)
				}
				mu.Lock()
				seen = append(seen, body["model"].(string))
				mu.Unlock()
				w.Write([]byte(tc.answer))
			}))
			defer server.Close()
			c := &Client{API: tc.api, BaseURL: server.URL, Model: "fast"}
			messages := []Message{{Role: "user", Content: "hi"}}
			for _, ctx := range []context.Context{t.Context(), WithModel(t.Context(), ""), WithModel(t.Context(), "smart")} {
				if _, err := c.Complete(ctx, messages, false); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{"fast", "fast", "smart"}
			if len(seen) != len(want) {
				t.Fatalf("models = %v", seen)
			}
			for i := range want {
				if seen[i] != want[i] {
					t.Fatalf("models = %v, want %v", seen, want)
				}
			}
			if c.Model != "fast" {
				t.Fatalf("override mutated client model: %q", c.Model)
			}
		})
	}
}

func TestConcurrentRequestsKeepTheirOwnModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
		}
		// The prompt carries the model the caller asked for.
		if want := body.Messages[0].Content; body.Model != want {
			t.Errorf("request for %q was sent with model %q", want, body.Model)
		}
		w.Write([]byte(chatAnswer))
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, Model: "fast"}
	var wg sync.WaitGroup
	for i := range 40 {
		model := "fast"
		ctx := t.Context()
		if i%2 == 0 {
			model = "smart"
			ctx = WithModel(ctx, model)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(ctx, []Message{{Role: "user", Content: model}}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if c.Model != "fast" {
		t.Fatalf("client model changed to %q", c.Model)
	}
}
