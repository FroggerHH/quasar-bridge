# TODO

## Proactive announcements (`POST /internal/announce`)

- Not verified on a real Yandex Station Lite: the unsolicited `AliceResponse` + `TtsGenerate`
  flow is modelled on the reply path and exercised only against simulated upstream frames.
  Check by eye that the speaker speaks, that `should_listen: true` reopens the microphone,
  and that `RefMessageId` set to the synthetic request id is accepted.
- Announcements are not queued: a busy speaker returns `409 speaker_busy` and the caller retries.
  An announcement that is still playing does not make the speaker "busy"; a second one sent
  right away would overlap it.
- Dialogue recording files the announcement under the speaker scope of the latest user request.
  Before the first request after a connect, it falls back to the connection scope, which a later
  device-scoped request will not see.
- Not documented in `docs/guide.md` yet.
