# Voice harness integration

This branch combines FroggerHH/main `08acdf5` with the five integration commits
from voice-harness (`90d9cbd` through `dba15fe`). Upstream diagnostics and audio
stream synchronization remain the base of the implementation.

## Configuration

The following members can coexist in config.json (merge them into an otherwise
valid deployment configuration):

```json
{
  "diagnostics": {"network_access": true, "port": 2704},
  "llm_system_prompt_file": "/etc/quasar-bridge/system-prompt.txt",
  "llm_model_routing": {
    "enabled": false,
    "smart_model": "",
    "triage_model": "",
    "triage_timeout_ms": 2500
  },
  "post_turn_hook": {
    "url": "http://127.0.0.1:8765/hooks/turn",
    "timeout_ms": 5000
  }
}
```

The prompt file must exist and contain nonblank text. Use either
`llm_system_prompt` or `llm_system_prompt_file`, never both. SIGHUP and the panel
reload action reread the file and update the prompt and model routing. Changing
the hook endpoint or diagnostics port requires a restart.

Open `http://<bridge-host>:2704/diagnostics/`. The network access switch retains
upstream's live rebind and request access guards. The local announcement API
remains on the original Bridge listener and is not exposed on the panel port.

## Preserved behavior and adaptations

- A custom system prompt replaces the default persona. Upstream's technical
  TTS vocabulary is still appended when markup is enabled; it does not restore
  the default persona. Hot reload uses atomic immutable snapshots.
- Default tool instructions are built from the tools available to that request.
  Upstream's explicit `assistant_tools.system_prompt` still overrides those
  defaults; runtime availability, privacy controls and call budgets remain.
- Fast/smart selection is disabled by default. When enabled, the selected model
  travels with the request context. Explicit selection also works with upstream
  ChatGPT subscription requests. Choosing a new base model in the panel updates
  the fast model and the implicit triage model without replacing the smart model.
- `POST /internal/announce` accepts loopback peers only, rejects forwarded
  requests and busy speakers, and retains announcement dialogue context.
  Its `delivered` flag reports the existing upstream-TTS acceptance condition;
  it is **not evidence of audible speaker playback**.
- The optional bounded hook worker receives final local `llm` and `home` turns
  through the existing capture path, without blocking speech. It does not receive
  Yandex turns or announcements. Hook failure does not fail the spoken answer.

## Home Assistant decision

The integration branch did not modify `internal/home`. The new upstream home
controller is retained. The hook's `home` route exists to let the external memory
service observe local home-control replies.

The separate Python voice-harness deployment deliberately uses HA
`media_player.play_media` with text for proactive notifications: the old Bridge
announcement path accepted TTS without reliably making an idle speaker speak.
The deployment handoff records real playback verification for the HA path.
Keep that choice until the Bridge path has its own device playback verification;
this source merge alone does not resolve or verify that failure.

## Validation

Tests cover prompt reload and model routing through the diagnostics apply API,
rollback on listener failure, preservation of hook/diagnostics settings,
subscription model override priority and changing fast models with an in-flight
router snapshot. The original hook, announcement and upstream tests remain.

```sh
go test -race ./... -count=1 -timeout=60s
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/quasar-bridge-port ./cmd/quasar-bridge
python3 scripts/check-public-tree.py
```

Concurrency uses the standard `sync/atomic` snapshots already used by this
project; the Go 1.27.4 memory-model documentation was checked through Context7.
No new dependencies or bespoke configuration framework were needed.

Verified locally on 2026-11-27: the full race suite passed in all 16 packages
with tests, `go vet`, the CGO-free build and indexed-tree scanner passed. A
temporary standalone process accepted the diagnostics block together with the
prompt file, disabled model routing and hook configuration, served HTML on port
2704 and returned `status=ok` and `active_sessions=0`. It was stopped after the
check. No live deployment or physical playback was changed or verified.
