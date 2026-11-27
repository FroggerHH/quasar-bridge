# Deployment verification after the voice-harness port

- [ ] Review and publish the integration branch into the fork's main branch.
- [ ] Before installing, compare the live binary/configuration with the local
  deployment inventory and preserve newer deployment changes.
- [ ] Install the verified build and verify diagnostics on port 2704 with the
  actual deployment configuration. Source tests do not verify the deployed service.
- [ ] Verify ordinary speaker speech and a home-control reply on the device;
  confirm playback events, not merely HTTP success.
- [ ] Keep proactive notifications on the established HA path. Separately verify
  idle-speaker playback and microphone reopening before switching to Bridge
  announcements. This merge does not establish either guarantee.

Integration decisions and verification commands: [docs/voice-harness.md](docs/voice-harness.md).
