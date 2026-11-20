package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

type announcerFunc func(context.Context, string, bool) (bridge.AnnounceResult, error)

func (f announcerFunc) Announce(ctx context.Context, text string, listen bool) (bridge.AnnounceResult, error) {
	return f(ctx, text, listen)
}

func postAnnounce(t *testing.T, a announcer, remote, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/announce", strings.NewReader(body))
	req.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/announce", announceHandler(a))
	mux.ServeHTTP(rec, req)
	return rec
}

func neverCalled(t *testing.T) announcer {
	return announcerFunc(func(context.Context, string, bool) (bridge.AnnounceResult, error) {
		t.Error("announcement attempted")
		return bridge.AnnounceResult{}, nil
	})
}

func TestAnnounceRejectsNonLoopbackPeers(t *testing.T) {
	for _, remote := range []string{"192.168.1.20:5555", "203.0.113.9:80", "[2001:db8::1]:80", "not-an-address"} {
		rec := postAnnounce(t, neverCalled(t), remote, `{"text":"привет"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d", remote, rec.Code)
		}
	}
}

func TestAnnounceRejectsProxiedRequests(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "Forwarded"} {
		rec := postAnnounce(t, neverCalled(t), "127.0.0.1:4000", `{"text":"привет"}`, header, "203.0.113.9")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d", header, rec.Code)
		}
	}
}

func TestAnnounceRejectsOtherMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/internal/announce", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/announce", announceHandler(neverCalled(t)))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestAnnounceRejectsBadBodies(t *testing.T) {
	for name, body := range map[string]string{
		"invalid JSON": `{"text":`,
		"wrong type":   `{"text":5}`,
		"oversized":    `{"text":"` + strings.Repeat("a", maxAnnounceBody) + `"}`,
	} {
		if rec := postAnnounce(t, neverCalled(t), "127.0.0.1:4000", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d", name, rec.Code)
		}
	}
	// Empty and over-long text are validated by the registry itself.
	registry := new(bridge.SessionRegistry)
	for name, body := range map[string]string{
		"empty":    `{"text":""}`,
		"blank":    `{"text":"  "}`,
		"missing":  `{"should_listen":true}`,
		"too long": `{"text":"` + strings.Repeat("я", bridge.MaxAnnouncementRunes+1) + `"}`,
	} {
		if rec := postAnnounce(t, registry, "127.0.0.1:4000", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d", name, rec.Code)
		}
	}
}

func TestAnnounceWithoutSessionIsUnavailable(t *testing.T) {
	for _, remote := range []string{"127.0.0.1:4000", "[::1]:4000", "[::ffff:127.0.0.1]:4000"} {
		rec := postAnnounce(t, new(bridge.SessionRegistry), remote, `{"text":"Готово","should_listen":true}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status=%d", remote, rec.Code)
		}
		var got bridge.AnnounceResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got != (bridge.AnnounceResult{Reason: "no_active_session"}) || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: body=%s", remote, rec.Body)
		}
	}
}

func TestAnnounceStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result bridge.AnnounceResult
		err    error
		status int
		body   string
	}{
		{"delivered", bridge.AnnounceResult{Delivered: true, ActiveSessions: 1}, nil, http.StatusOK, `{"delivered":true,"active_sessions":1}`},
		{"busy", bridge.AnnounceResult{ActiveSessions: 1, Reason: "speaker_busy"}, nil, http.StatusConflict, `{"delivered":false,"active_sessions":1,"reason":"speaker_busy"}`},
		{"speech failed", bridge.AnnounceResult{ActiveSessions: 1, Reason: "speech_failed"}, errors.New("internal detail"), http.StatusBadGateway, `{"delivered":false,"active_sessions":1,"reason":"speech_failed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotText string
			var gotListen bool
			rec := postAnnounce(t, announcerFunc(func(_ context.Context, text string, listen bool) (bridge.AnnounceResult, error) {
				gotText, gotListen = text, listen
				return tc.result, tc.err
			}), "127.0.0.1:4000", `{"text":"Я закончила задачу про рецепты блинов.","should_listen":true}`)
			if rec.Code != tc.status || strings.TrimSpace(rec.Body.String()) != tc.body {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if gotText != "Я закончила задачу про рецепты блинов." || !gotListen {
				t.Fatalf("request not passed through: %q %v", gotText, gotListen)
			}
		})
	}
}
