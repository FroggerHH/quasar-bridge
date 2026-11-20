package bridge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/dialogue"
)

const (
	defaultPostTurnHookTimeoutMS = 5000
	minPostTurnHookTimeoutMS     = 200
	maxPostTurnHookTimeoutMS     = 30000
	postTurnHookQueueSize        = 32
)

// PostTurnHookConfig describes an optional local HTTP callback invoked after a
// user question and its LLM or Home Assistant answer were delivered to the speaker.
type PostTurnHookConfig struct {
	// URL is an http or https endpoint. Empty disables the hook.
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms"`
}

func DefaultPostTurnHookConfig() PostTurnHookConfig {
	return PostTurnHookConfig{TimeoutMS: defaultPostTurnHookTimeoutMS}
}

func (c PostTurnHookConfig) Enabled() bool { return c.URL != "" }

// Validate accepts a disabled hook as is; an enabled one needs an http(s)
// endpoint without credentials and a timeout between 200 and 30000 ms.
func (c PostTurnHookConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("post_turn_hook.url must be an http or https URL")
	}
	if c.TimeoutMS < minPostTurnHookTimeoutMS || c.TimeoutMS > maxPostTurnHookTimeoutMS {
		return fmt.Errorf("post_turn_hook.timeout_ms must be between %d and %d", minPostTurnHookTimeoutMS, maxPostTurnHookTimeoutMS)
	}
	return nil
}

type postTurnPayload struct {
	Timestamp     string `json:"timestamp"`
	SpeakerID     string `json:"speaker_id"`
	Route         string `json:"route"`
	UserText      string `json:"user_text"`
	AssistantText string `json:"assistant_text"`
}

// PostTurnHook delivers turn notifications from one background worker through a
// bounded queue, so a slow or unreachable endpoint never delays the speaker and
// never accumulates goroutines. A nil *PostTurnHook ignores every call.
type PostTurnHook struct {
	url    string
	client *http.Client
	events chan postTurnPayload
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.RWMutex // guards closed against sends on the closed channel
	closed bool
}

// NewPostTurnHook returns nil, nil for a disabled configuration.
func NewPostTurnHook(cfg PostTurnHookConfig) (*PostTurnHook, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &PostTurnHook{
		url: cfg.URL,
		client: &http.Client{
			Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond,
			// A redirect would resend the conversation to a host nobody configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		events: make(chan postTurnPayload, postTurnHookQueueSize),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go h.run()
	return h, nil
}

// Notify queues a delivered turn and returns immediately. Only completed
// llm and home turns with both texts are sent; the event is dropped when the
// queue is full.
func (h *PostTurnHook) Notify(ctx context.Context, route, question, answer string, final bool) {
	if h == nil || !final || (route != "llm" && route != "home") {
		return
	}
	question, answer = strings.TrimSpace(question), strings.TrimSpace(StripTTSMarkup(answer))
	if question == "" || answer == "" {
		return
	}
	event := postTurnPayload{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		SpeakerID:     dialogue.SpeakerKey(ctx),
		Route:         route,
		UserText:      question,
		AssistantText: answer,
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return
	}
	select {
	case h.events <- event:
	default:
		slog.Warn("post_turn_hook_dropped", "reason", "queue full")
	}
}

// Close stops the worker, abandons queued events and aborts a request in flight.
func (h *PostTurnHook) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.events)
	}
	h.mu.Unlock()
	h.cancel()
	<-h.done
}

func (h *PostTurnHook) run() {
	defer close(h.done)
	for event := range h.events {
		if h.ctx.Err() != nil {
			continue
		}
		h.send(event)
	}
}

func (h *PostTurnHook) send(event postTurnPayload) {
	body, err := json.Marshal(event)
	if err != nil {
		slog.Warn("post_turn_hook_failed", "reason", "encode")
		return
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("post_turn_hook_failed", "reason", "request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		if h.ctx.Err() != nil {
			return
		}
		// url.Error carries the configured URL, which may hold a secret; keep the cause only.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		slog.Warn("post_turn_hook_failed", "reason", "request failed", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		slog.Warn("post_turn_hook_failed", "reason", "unexpected status", "status", resp.StatusCode)
	}
}
