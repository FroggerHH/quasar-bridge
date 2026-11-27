package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"

	"github.com/skrashevich/quasar-bridge/internal/bridge"
)

const maxAnnounceBody = 16 << 10

type announcer interface {
	Announce(ctx context.Context, text string, shouldListen bool) (bridge.AnnounceResult, error)
}

type announceRequest struct {
	Text         string `json:"text"`
	ShouldListen bool   `json:"should_listen"`
}

// fromLoopback reports whether the TCP peer is a loopback address. It ignores
// allowed_ips: announcements are for services on the same machine only.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// announceHandler serves POST /internal/announce. A reverse proxy on this host
// would make every client look like loopback, so proxied requests are refused.
func announceHandler(a announcer) http.HandlerFunc {
	reply := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		data, _ := json.Marshal(body)
		_, _ = w.Write(data)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req announceRequest
		if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxAnnounceBody), &req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		result, err := a.Announce(r.Context(), req.Text, req.ShouldListen)
		switch {
		case errors.Is(err, bridge.ErrInvalidAnnouncement):
			http.Error(w, "text must be non-empty and at most 4000 characters", http.StatusBadRequest)
		case err != nil:
			slog.Warn("announce_failed", "error", err)
			reply(w, http.StatusBadGateway, result)
		case result.Delivered:
			reply(w, http.StatusOK, result)
		case result.Reason == bridge.ReasonSpeakerBusy:
			reply(w, http.StatusConflict, result)
		default:
			reply(w, http.StatusServiceUnavailable, result)
		}
	}
}
