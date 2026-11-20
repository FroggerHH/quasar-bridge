package bridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	directive "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/wire"
)

// MaxAnnouncementRunes bounds one proactive announcement.
const MaxAnnouncementRunes = 4000

// announceSpeechTimeout bounds how long Announce waits for upstream TTS to
// start speaking. It is a variable so tests can shorten it.
var announceSpeechTimeout = 15 * time.Second

// announcementTTL removes announcement bookkeeping when the speech stream never
// closes, mirroring the 90-second lifetime of a native request.
const announcementTTL = 90 * time.Second

// announcementDialogueUser stands in for the user turn of a recorded announcement,
// because dialogue history stores user/assistant pairs.
const announcementDialogueUser = "(фоновое уведомление ассистента)"

var (
	ErrInvalidAnnouncement = errors.New("announcement text is empty or too long")
	ErrSpeakerBusy         = errors.New("speaker is handling a user request")
	ErrSessionClosed       = errors.New("speaker session is closed")
	ErrAnnounceTimeout     = errors.New("speech did not start in time")
)

// AnnounceResult is the outcome of one proactive announcement attempt.
type AnnounceResult struct {
	Delivered      bool   `json:"delivered"`
	ActiveSessions int    `json:"active_sessions"`
	Reason         string `json:"reason,omitempty"`
}

const (
	ReasonNoActiveSession = "no_active_session"
	ReasonSpeakerBusy     = "speaker_busy"
	ReasonSpeechFailed    = "speech_failed"
)

// SessionRegistry tracks connected speaker sessions so local services can speak
// on the most recently connected one.
type SessionRegistry struct {
	mu       sync.Mutex
	sessions []*Native
}

// Register adds n; Native.Close removes it again.
func (r *SessionRegistry) Register(n *Native) {
	n.mu.Lock()
	n.registry = r
	n.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s == n {
			return
		}
	}
	r.sessions = append(r.sessions, n)
}

func (r *SessionRegistry) Unregister(n *Native) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.sessions {
		if s == n {
			r.sessions = append(r.sessions[:i:i], r.sessions[i+1:]...)
			return
		}
	}
}

func (r *SessionRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *SessionRegistry) latest() (*Native, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) == 0 {
		return nil, 0
	}
	return r.sessions[len(r.sessions)-1], len(r.sessions)
}

// Announce speaks text on the most recently registered session. A missing or
// busy speaker is reported in the result, not as an error, so callers can retry.
func (r *SessionRegistry) Announce(ctx context.Context, text string, shouldListen bool) (AnnounceResult, error) {
	if err := validateAnnouncement(text); err != nil {
		return AnnounceResult{Reason: "invalid_text"}, err
	}
	n, count := r.latest()
	if n == nil {
		return AnnounceResult{Reason: ReasonNoActiveSession}, nil
	}
	err := n.Announce(ctx, text, shouldListen)
	switch {
	case err == nil:
		return AnnounceResult{Delivered: true, ActiveSessions: count}, nil
	case errors.Is(err, ErrSpeakerBusy):
		return AnnounceResult{ActiveSessions: count, Reason: ReasonSpeakerBusy}, nil
	case errors.Is(err, ErrSessionClosed):
		// Closed between lookup and use: indistinguishable from no speaker.
		return AnnounceResult{ActiveSessions: count - 1, Reason: ReasonNoActiveSession}, nil
	}
	return AnnounceResult{ActiveSessions: count, Reason: ReasonSpeechFailed}, err
}

func validateAnnouncement(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > MaxAnnouncementRunes {
		return ErrInvalidAnnouncement
	}
	return nil
}

// Announce speaks text without a preceding user request and, if shouldListen,
// asks the Station to open the microphone afterwards. It returns once upstream
// TTS has started speaking, so a nil error means audio is on its way.
//
// If the speaker is handling a user request it returns ErrSpeakerBusy instead of
// interrupting. Recorded dialogue context (when the resolver supports it) lets a
// reply such as "озвучь" refer to what was just announced.
func (n *Native) Announce(ctx context.Context, text string, shouldListen bool) error {
	if err := validateAnnouncement(text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	var spoken Resolution
	spoken.setModelAnswer(text, n.TTSMarkup)
	speech := cmp.Or(spoken.speech, spoken.Answer)

	requestID, responseID, ttsID := newID(), newID(), newID()
	pctx, cancel := context.WithCancel(n.ctx)
	p := &nativeRequest{
		id:           requestID,
		ttsID:        ttsID,
		ctx:          pctx,
		cancel:       cancel,
		local:        true,
		announcement: true,
		speaking:     make(chan struct{}),
		request:      &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new(requestID)}},
	}

	n.mu.Lock()
	switch {
	case n.ctx.Err() != nil:
		n.mu.Unlock()
		cancel()
		return ErrSessionClosed
	case len(n.requests) > 0 || n.announcing:
		n.mu.Unlock()
		cancel()
		return ErrSpeakerBusy
	}
	n.announcing = true
	n.tts[ttsID] = p
	n.announcements[ttsID] = p
	p.expires = time.AfterFunc(announcementTTL, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.dropAnnouncementLocked(p)
	})
	scope := n.speakerScope
	n.mu.Unlock()
	// Entries stay registered after success: the TtsSpeak, Opus chunks and
	// StreamControl of this announcement are matched through them until expiry.
	started := false
	defer func() {
		n.mu.Lock()
		n.announcing = false
		if !started {
			// Keep the canceled entry so a late TtsSpeak is dropped, not spoken.
			p.canceled = true
			p.cancel()
		}
		n.mu.Unlock()
		if !started {
			_ = n.sendCancel(n.ctx, ttsID)
		}
	}()

	frame := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: requestID}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{
		Header:        &response.TAliceResponseProto_THeader{RequestId: new(requestID), ResponseId: new(responseID)},
		VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(spoken.Answer)}, HasVoiceResponse: true, ShouldListen: shouldListen},
		Response: &response.TAliceResponseProto_TResponse{
			Cards:      []*response.TAliceResponseProto_TResponse_TCard{{Type: "simple_text", Text: new(spoken.Answer), CardId: new(newID())}},
			Directives: []*directive.TDirective{{Type: "client_action", Name: "tts_play_placeholder"}},
		},
	}}}}}
	b, err := (wire.Frame{Server: frame, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.down(pctx, websocket.MessageBinary, b); err != nil {
		return fmt.Errorf("send announcement: %w", err)
	}
	if err := n.synthesize(p, speech); err != nil {
		return fmt.Errorf("request announcement speech: %w", err)
	}

	timer := time.NewTimer(announceSpeechTimeout)
	defer timer.Stop()
	select {
	case <-p.speaking:
	case <-ctx.Done():
		return ctx.Err()
	case <-pctx.Done():
		return ErrSessionClosed
	case <-timer.C:
		return ErrAnnounceTimeout
	}
	n.mu.Lock()
	started = !p.canceled
	n.mu.Unlock()
	if !started {
		return errors.New("announcement speech was rejected")
	}
	slog.Info("native_announce", "request", requestTag(requestID), "should_listen", shouldListen, "text_bytes", len(text))
	if recorder, ok := n.resolver.(interface {
		RecordAnnouncement(context.Context, string)
	}); ok {
		recorder.RecordAnnouncement(dialogue.WithSpeaker(n.ctx, cmp.Or(scope, "connection:"+n.connectionScope)), spoken.Answer)
	}
	return nil
}

// dropAnnouncementLocked forgets p; callers hold n.mu.
func (n *Native) dropAnnouncementLocked(p *nativeRequest) {
	p.canceled = true
	p.cancel()
	p.expires.Stop()
	delete(n.announcements, p.ttsID)
	delete(n.tts, p.ttsID)
	for stream, q := range n.streams {
		if q == p {
			delete(n.streams, stream)
		}
	}
	for stream, q := range n.blocked {
		if q == p {
			delete(n.blocked, stream)
		}
	}
}

// RecordAnnouncement stores an announcement as the latest assistant turn of the
// speaker's dialogue, so a short reply ("озвучь", "скинь в телеграм") keeps its
// referent. It is a no-op while dialogue context is disabled.
func (e *Engine) RecordAnnouncement(ctx context.Context, text string) {
	store := e.dialogue()
	_, ticket := store.Begin(ctx)
	store.Commit(ctx, ticket, announcementDialogueUser, text, false)
}
