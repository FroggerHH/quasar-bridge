package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stream "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"github.com/skrashevich/quasar-bridge/internal/dialogue"
	"github.com/skrashevich/quasar-bridge/internal/wire"
)

type announceHarness struct {
	n        *Native
	registry *SessionRegistry
	up, down chan []byte
}

func newAnnounceHarness(t *testing.T, resolver Resolver) *announceHarness {
	t.Helper()
	h := &announceHarness{registry: new(SessionRegistry), up: make(chan []byte, 32), down: make(chan []byte, 32)}
	h.n = NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, b []byte) error {
		h.up <- append([]byte(nil), b...)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, b []byte) error {
		h.down <- append([]byte(nil), b...)
		return nil
	}, resolver)
	h.registry.Register(h.n)
	t.Cleanup(h.n.Close)
	return h
}

func (h *announceHarness) nextUp(t *testing.T) wire.Frame {
	t.Helper()
	select {
	case b := <-h.up:
		return wire.Decode(b, wire.ClientToServer)
	case <-time.After(time.Second):
		t.Fatal("nothing sent upstream")
		return wire.Frame{}
	}
}

func (h *announceHarness) nextDown(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-h.down:
		return b
	case <-time.After(time.Second):
		t.Fatal("nothing sent downstream")
		return nil
	}
}

func (h *announceHarness) assertSilent(t *testing.T, what string) {
	t.Helper()
	select {
	case b := <-h.down:
		t.Fatalf("%s: unexpected downstream frame (%d bytes)", what, len(b))
	case <-time.After(50 * time.Millisecond):
	}
}

// speak plays the upstream side of one TTS synthesis for ttsID.
func (h *announceHarness) speak(ctx context.Context, ttsID string, streamID int64, chunks ...string) error {
	speak := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: ttsID, StreamId: new(streamID)}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts.TTtsSpeak{Format: "audio/opus"}}}}}
	if err := h.n.Server(ctx, websocket.MessageBinary, aa(speak)); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if err := h.n.Server(ctx, websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, uint32(streamID)), chunk...)); err != nil {
			return err
		}
	}
	closed := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{MessageId: ttsID, StreamId: streamID, Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}}}
	return h.n.Server(ctx, websocket.MessageBinary, aa(closed))
}

func TestRegistryWithoutSessionsReportsNoActiveSession(t *testing.T) {
	r := new(SessionRegistry)
	result, err := r.Announce(t.Context(), "Готово", true)
	if err != nil {
		t.Fatal(err)
	}
	if result != (AnnounceResult{Reason: "no_active_session"}) {
		t.Fatalf("result=%+v", result)
	}
}

func TestRegistryRejectsInvalidText(t *testing.T) {
	h := newAnnounceHarness(t, nil)
	for name, text := range map[string]string{"empty": "", "blank": " \n\t ", "too long": strings.Repeat("я", MaxAnnouncementRunes+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.registry.Announce(t.Context(), text, false); !errors.Is(err, ErrInvalidAnnouncement) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	h.assertSilent(t, "invalid text")
}

func TestRegistryUnregistersOnClose(t *testing.T) {
	r := new(SessionRegistry)
	first := NewNative(t.Context(), nil, nil, nil)
	second := NewNative(t.Context(), nil, nil, nil)
	r.Register(first)
	r.Register(second)
	r.Register(second) // Registering twice must not duplicate the session.
	if got := r.Count(); got != 2 {
		t.Fatalf("count=%d", got)
	}
	if latest, _ := r.latest(); latest != second {
		t.Fatal("most recent session is not preferred")
	}
	second.Close()
	if latest, count := r.latest(); latest != first || count != 1 {
		t.Fatalf("after close latest=%v count=%d", latest == first, count)
	}
	first.Close()
	first.Close() // Close stays idempotent.
	if got := r.Count(); got != 0 {
		t.Fatalf("count=%d", got)
	}
}

func TestAnnounceDoesNotInterruptActiveRequest(t *testing.T) {
	h := newAnnounceHarness(t, nil)
	pending := &nativeRequest{id: "user-request", ctx: t.Context(), cancel: func() {}, expires: time.NewTimer(time.Hour)}
	h.n.requests[pending.id] = pending
	result, err := h.registry.Announce(t.Context(), "Готово", true)
	if err != nil {
		t.Fatal(err)
	}
	if result != (AnnounceResult{ActiveSessions: 1, Reason: "speaker_busy"}) {
		t.Fatalf("result=%+v", result)
	}
	if h.n.requests[pending.id] != pending || pending.canceled {
		t.Fatal("announcement clobbered the active request")
	}
	h.assertSilent(t, "busy speaker")
	select {
	case <-h.up:
		t.Fatal("busy speaker received an upstream frame")
	default:
	}
	if len(h.n.announcements) != 0 || len(h.n.tts) != 0 {
		t.Fatal("busy attempt left bookkeeping behind")
	}
}

func TestAnnounceSpeaksAndOpensMicrophone(t *testing.T) {
	for _, listen := range []bool{true, false} {
		t.Run(map[bool]string{true: "listen", false: "silent"}[listen], func(t *testing.T) {
			h := newAnnounceHarness(t, nil)
			text := "Я закончила задачу про рецепты блинов. Озвучить итог сейчас или скинуть в Телеграм?"
			type outcome struct {
				result AnnounceResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := h.registry.Announce(t.Context(), text, listen)
				done <- outcome{result, err}
			}()

			var alice *server.TServerMessage_TEvent
			frame := wire.Decode(h.nextDown(t), wire.ServerToClient)
			if frame.Kind() != "AliceResponse" {
				t.Fatalf("first downstream frame is %s", frame.Kind())
			}
			alice = frame.Server.GetEvent()
			r := alice.GetAliceResponse()
			if r.GetVoiceResponse().GetShouldListen() != listen || !r.GetVoiceResponse().GetHasVoiceResponse() || r.GetVoiceResponse().GetOutputSpeech().GetText() != text {
				t.Fatalf("voice response=%v", r.GetVoiceResponse())
			}
			if cards := r.GetResponse().GetCards(); len(cards) != 1 || cards[0].GetType() != "simple_text" || cards[0].GetText() != text {
				t.Fatalf("cards=%v", cards)
			}
			if ds := r.GetResponse().GetDirectives(); len(ds) != 1 || ds[0].GetType() != "client_action" || ds[0].GetName() != "tts_play_placeholder" {
				t.Fatalf("directives=%v", ds)
			}
			requestID, responseID := r.GetHeader().GetRequestId(), r.GetHeader().GetResponseId()
			if requestID == "" || responseID == "" || requestID == responseID {
				t.Fatalf("header=%v", r.GetHeader())
			}

			generate := h.nextUp(t)
			if generate.Kind() != "TtsGenerate" {
				t.Fatalf("upstream frame is %s", generate.Kind())
			}
			event := generate.Client.GetEvent()
			ttsID := event.GetHeader().GetMessageId()
			if g := event.GetTtsGenerate(); g.GetText() != text || g.GetLang() != "ru-RU" || g.GetFormat() != "audio/opus" || ttsID == "" {
				t.Fatalf("generate=%v", g)
			}

			select {
			case o := <-done:
				t.Fatalf("Announce returned before speech started: %+v %v", o.result, o.err)
			case <-time.After(30 * time.Millisecond):
			}
			if err := h.speak(t.Context(), ttsID, 72, "opus-one", "opus-two"); err != nil {
				t.Fatal(err)
			}

			speakFrame := wire.Decode(h.nextDown(t), wire.ServerToClient)
			speak := speakFrame.Server.GetEvent()
			if speakFrame.Kind() != "TtsSpeak" || speak.GetHeader().GetRefMessageId() != requestID {
				t.Fatalf("TtsSpeak not forwarded for the announcement request: %s ref=%q", speakFrame.Kind(), speak.GetHeader().GetRefMessageId())
			}
			for _, want := range []string{"opus-one", "opus-two"} {
				chunk := h.nextDown(t)
				if binary.BigEndian.Uint32(chunk) != 72 || string(chunk[4:]) != want {
					t.Fatalf("audio chunk=%q", chunk)
				}
			}
			closeFrame := wire.Decode(h.nextDown(t), wire.ServerToClient)
			if sc := closeFrame.Server.GetStreamControl(); sc == nil || sc.GetClose() == nil || sc.GetMessageId() != requestID || sc.GetStreamId() != 72 {
				t.Fatalf("stream control=%v", closeFrame.Server)
			}

			select {
			case o := <-done:
				if o.err != nil || o.result != (AnnounceResult{Delivered: true, ActiveSessions: 1}) {
					t.Fatalf("result=%+v err=%v", o.result, o.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Announce did not return after speech started")
			}

			h.n.Close()
			if h.registry.Count() != 0 {
				t.Fatal("closed session is still registered")
			}
			h.n.mu.Lock()
			defer h.n.mu.Unlock()
			if len(h.n.announcements) != 0 || len(h.n.tts) != 0 || len(h.n.streams) != 0 {
				t.Fatalf("Close left announcement state: %d %d %d", len(h.n.announcements), len(h.n.tts), len(h.n.streams))
			}
		})
	}
}

func TestAnnounceReportsSpeechTimeoutAndStaysSilentAfterwards(t *testing.T) {
	previous := announceSpeechTimeout
	announceSpeechTimeout = 40 * time.Millisecond
	t.Cleanup(func() { announceSpeechTimeout = previous })
	h := newAnnounceHarness(t, nil)

	result, err := h.registry.Announce(t.Context(), "Готово", true)
	if !errors.Is(err, ErrAnnounceTimeout) || result.Delivered || result.Reason != "speech_failed" || result.ActiveSessions != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	<-h.down // The AliceResponse was already sent.
	generate := h.nextUp(t)
	if generate.Kind() != "TtsGenerate" {
		t.Fatalf("upstream frame is %s", generate.Kind())
	}
	if cancel := h.nextUp(t); cancel.Kind() != "CancelRequest" {
		t.Fatalf("timeout should cancel upstream speech, got %s", cancel.Kind())
	}

	// Upstream answers after the caller was told the announcement failed.
	if err := h.speak(t.Context(), generate.Client.GetEvent().GetHeader().GetMessageId(), 9, "late"); err != nil {
		t.Fatal(err)
	}
	h.assertSilent(t, "late speech after timeout")

	// A failed announcement must not wedge the speaker.
	go func() { _ = h.speak(t.Context(), h.nextUp(t).Client.GetEvent().GetHeader().GetMessageId(), 10) }()
	if result, err := h.registry.Announce(t.Context(), "Ещё раз", false); err != nil || !result.Delivered {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
}

func TestAnnounceStopsWhenCallerGivesUp(t *testing.T) {
	h := newAnnounceHarness(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	errs := make(chan error, 1)
	go func() { errs <- h.n.Announce(ctx, "Готово", false) }()
	h.nextDown(t)
	h.nextUp(t)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestAnnounceAfterCloseReportsNoActiveSession(t *testing.T) {
	h := newAnnounceHarness(t, nil)
	h.n.Close()
	if err := h.n.Announce(t.Context(), "Готово", false); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err=%v", err)
	}
	result, err := h.registry.Announce(t.Context(), "Готово", false)
	if err != nil || result != (AnnounceResult{Reason: "no_active_session"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAnnounceTTSMarkup(t *testing.T) {
	const text = `Блины <speaker is_whisper="true"> готовы, зам+ок открыт`
	for _, keep := range []bool{true, false} {
		t.Run(map[bool]string{true: "markup", false: "plain"}[keep], func(t *testing.T) {
			h := newAnnounceHarness(t, nil)
			h.n.TTSMarkup = keep
			synthesized := make(chan string, 1)
			go func() {
				generate := h.nextUp(t)
				synthesized <- generate.Client.GetEvent().GetTtsGenerate().GetText()
				// Marked-up speech is re-chunked into chained Opus streams; the
				// announcement only needs the first TtsSpeak to be accepted.
				_ = h.speak(context.Background(), generate.Client.GetEvent().GetHeader().GetMessageId(), 5)
			}()
			if err := h.n.Announce(t.Context(), text, false); err != nil {
				t.Fatal(err)
			}
			shown := wire.Decode(h.nextDown(t), wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText()
			if shown != "Блины готовы, замок открыт" {
				t.Fatalf("displayed text keeps markup: %q", shown)
			}
			spoken := <-synthesized
			if strings.Contains(spoken, "<speaker") != keep || strings.Contains(spoken, "зам+ок") != keep {
				t.Fatalf("synthesized text=%q, keep markup=%v", spoken, keep)
			}
		})
	}
}

func TestNativeRemembersSpeakerScopeOfLatestRequest(t *testing.T) {
	h := newAnnounceHarness(t, nil)
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "earlier"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Application: &app.TClientInfoProto{DeviceId: new("station-lite")}, Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("привет")}}}}}}}
	if err := h.n.Client(t.Context(), websocket.MessageBinary, aa(input)); err != nil {
		t.Fatal(err)
	}
	h.n.mu.Lock()
	defer h.n.mu.Unlock()
	if h.n.speakerScope != "device:station-lite" {
		t.Fatalf("speaker scope=%q", h.n.speakerScope)
	}
}

type recordingResolver struct {
	engine *Engine
}

func (r recordingResolver) Resolve(ctx context.Context, text string) (Resolution, error) {
	return r.engine.Resolve(ctx, text)
}
func (r recordingResolver) RecordAnnouncement(ctx context.Context, text string) {
	r.engine.RecordAnnouncement(ctx, text)
}

func TestAnnounceRecordsDialogueContextForFollowUp(t *testing.T) {
	engine := &Engine{Dialogue: dialogue.New(dialogue.DefaultConfig())}
	h := newAnnounceHarness(t, recordingResolver{engine})

	// Announce files the turn under the speaker of the latest user request.
	const device = "station-lite"
	h.n.speakerScope = "device:" + device

	go func() { _ = h.speak(context.Background(), h.nextUp(t).Client.GetEvent().GetHeader().GetMessageId(), 3) }()
	const text = "Задача про блины готова. Озвучить итог?"
	if err := h.n.Announce(t.Context(), text, true); err != nil {
		t.Fatal(err)
	}
	view := engine.Dialogue.View(dialogue.WithSpeaker(t.Context(), "device:"+device))
	if len(view.Messages) != 2 || view.Messages[0].Role != "user" || view.Messages[1].Role != "assistant" || view.Messages[1].Content != text {
		t.Fatalf("dialogue after announcement=%+v", view.Messages)
	}
	other := engine.Dialogue.View(dialogue.WithSpeaker(t.Context(), "device:other"))
	if len(other.Messages) != 0 {
		t.Fatal("announcement leaked into another speaker's dialogue")
	}
}
