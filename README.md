# paul

Persistent memory for Claude Code. paul indexes conversations, distills them into memories (topics, decisions, pending items, facts) attached to subjects, and reports them to Claude when they come up: at session start, then on every message, with the memories closest to it.

The memory stays on the machine, in `~/.claude/paul/`, but the text of conversations, with secrets masked, is sent to the model to be summarized (see "Summaries"). Secrets found in messages are masked before indexing.

## Installation

In a terminal (or, in Claude Code, with `/plugin marketplace add` and `/plugin install`):

```
claude plugin marketplace add jplot/brain
claude plugin install paul@jplot
```

On the first session start, the plugin downloads the binary for the platform in the background (macOS or Linux, arm64 or amd64) and verifies its checksum: that first session runs without memory. The `paul` command appears in `~/.local/bin` when the next session starts; that folder must be in the `PATH`. Change this location with `PAUL_BIN_DIR`. Dependencies: `claude`, `git`, `curl`, `shasum` or `sha256sum`.

No setup is needed. Optional variables: `PAUL_HOME` (default `~/.claude/paul/`) and `PAUL_TRANSCRIPTS` (default `~/.claude/projects/`).

## Summaries

From the second session on, each session is summarized in the background by `claude -p` with Haiku, and subject syntheses are written with Sonnet. The model receives the masked text of the conversations and the memories drawn from them. A summary costs about $0.003 per session (measured). A synthesis costs more, since the model is Sonnet, and each pass writes at most 10.

## Commands

```
paul search <words>             --subject --since --until --limit
paul souvenirs [words]          --kind --subject --open --history --limit
paul session <id>
paul day <YYYY-MM-DD>
paul subjects [words]           the subject tree and its links
paul memo                       --subject
paul summarize                  --session --limit --all --force
paul forget point|session <id>  --yes
paul index
paul rebuild
paul web                        --no-open
```

- `--kind` takes `topic`, `decision`, `pending` or `fact`.
- `paul forget` describes what would be erased and stops with exit code 2 until `--yes` is given.
- `paul web` opens a local page (127.0.0.1): session feed, search, memories with correction and forgetting, subject tree.
- The `paul:souvenirs` skill lets Claude search the memories whenever the user talks about the past.

## External journals, format v1

Any `*.jsonl` file placed in `~/.claude/paul/journals/` is indexed as a conversation. One JSON line per message:

```
v        1
id       string, unique within the file
session  session identifier
ts       ISO 8601 UTC timestamp
role     "user" | "claude" | "subagent" | "tool"
text     the message; paul masks it at indexing
title    optional; session title
cwd      optional; repository hint for the summarizer
```
