# claude-code stream fixtures

`stream-success.jsonl` is a real `claude -p --output-format stream-json --verbose`
run (claude **2.1.221**, `--model haiku`, prompt "Reply with exactly: hi"),
with machine-specific fields stripped: the init line's `cwd`, `memory_paths`,
`tools`, `skills`, `slash_commands`, `mcp_servers`, `plugins`, and `agents`, and
the thinking block's text and signature. Session ids are replaced with a fixed
UUID.

Every field the adapter reads is verbatim from the recording — `type`,
`subtype`, `session_id`, `message.content[].type/text`, `usage.*`,
`total_cost_usd`, `is_error`, `api_error_status`. Re-record with the same
command when the harness changes shape; the point of the fixture is that the
translator is tested against what the harness emits, not against what we
remember it emitting (SPEC §12.2).
