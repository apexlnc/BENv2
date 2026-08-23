# codex-exec stream fixtures

Both files are real `codex exec --json` runs (codex-cli **0.147.0**), with
thread ids replaced by a fixed UUID and the per-request correlation ids
(`cf-ray`, `request id`) stripped from the recorded prose. Nothing else is
edited.

- `stream-success.jsonl` — `--sandbox workspace-write`, prompt "Run the shell
  command: echo hi. Then reply with the word done.", inside a git repository.
  It covers the shapes the translator has to tell apart: `thread.started`,
  `turn.started`, an `agent_message` item, a `command_execution` item in both
  its `item.started` and `item.completed` forms, and `turn.completed` with the
  usage object.
- `stream-auth-failure.jsonl` — the same command against an empty `CODEX_HOME`
  with an unusable key, trimmed from eleven near-identical reconnect notices to
  the distinct shapes. It is the fixture behind the `auth` verdict: `error`
  lines and an `error` *item* appear mid-run and the turn continues past them,
  so only the closing `turn.failed` may end the run — and its message text is
  the only discriminator the stream offers (SPEC §7.3).

Re-record with the same commands when the harness changes shape; the point of a
fixture is that the translator is tested against what the harness emits, not
against what we remember it emitting (SPEC §12.2).
