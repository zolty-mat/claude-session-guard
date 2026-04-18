# claude-session-guard

Advisory file-level coordination for parallel [Claude Code](https://claude.com/claude-code) sessions — so three tabs don't silently overwrite each other's work.

Hooks into Claude Code's `SessionStart`, `SessionEnd`, and `PreToolUse` events. Tracks which session is editing which file. Injects a warning into the LLM's own context when two sessions are about to clobber the same file within 10 minutes. Optionally mirrors session timelines to a Slack thread so you can watch them in real time from your phone.

Single Go binary. No runtime deps. ~5ms per hook. Slack is opt-in.

## Why this exists

If you run one Claude Code tab, you don't need this. If you run three or five across different repos, and one of them starts tidying up the shared `docs/` folder while another is mid-refactor in the same file, the last writer wins — silently. The tabs don't know about each other.

`claude-session-guard` gives each session a minimal awareness of its siblings. Every time any session is about to `Edit` or `Write` a file, it checks a local state directory for recent claims by *other* sessions. If another session touched the same file in the last 10 minutes, the hook returns `additionalContext` to the LLM telling it to pause and coordinate. Claude sees the warning and usually stops to ask you before overwriting.

The Slack mirror is the nice-to-have — you get a timeline thread per session with 🟢 start / ✏️ edits / ⚠️ conflicts / ✅ end, so you can glance at your phone from the couch and know what's happening in the tabs upstairs.

## Install

Requires Go 1.22+.

```bash
git clone https://github.com/zolty-mat/claude-session-guard.git
cd claude-session-guard
make install         # installs to ~/.local/bin/claude-session-guard
```

Make sure `~/.local/bin` is on your `PATH`.

## Wire it into Claude Code

Copy `examples/settings.hooks.json` into your `~/.claude/settings.json` (or project-level `.claude/settings.json`). If the file already has content, merge the `hooks` block.

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [{ "type": "command", "command": "claude-session-guard start" }] }
    ],
    "SessionEnd": [
      { "hooks": [{ "type": "command", "command": "claude-session-guard stop" }] }
    ],
    "PreToolUse": [
      {
        "matcher": "Edit|Write|NotebookEdit",
        "hooks": [{ "type": "command", "command": "claude-session-guard pre-edit" }]
      }
    ]
  }
}
```

Start a new Claude Code session. Run `claude-session-guard status` in a terminal — you should see your session listed.

## Enabling Slack (optional)

Without a token, the guard runs local-only. The LLM still gets the `additionalContext` conflict warnings — you just don't get the timeline thread.

1. Create a Slack app at [api.slack.com/apps](https://api.slack.com/apps) → "From scratch."
2. Add bot token scopes: `chat:write`, `reactions:write`.
3. Install to your workspace. Grab the `xoxb-...` bot token.
4. Invite the bot to a channel (e.g. `/invite @claude-sessions` in `#claude-sessions`).
5. Get the channel ID from the channel's **About** panel.
6. Drop the values in the config file:

   ```bash
   mkdir -p ~/.local/share/claude-session-guard
   cp examples/config.env.example ~/.local/share/claude-session-guard/config.env
   # edit the file, fill in SLACK_BOT_TOKEN and SLACK_CHANNEL_ID
   ```

7. Verify: `claude-session-guard test` should post a `🧪 hook test` message.

Environment variables (`SLACK_BOT_TOKEN`, `SLACK_CHANNEL_ID`) take precedence over the config file, so you can also export them from your shell profile if you prefer.

## How conflict detection works

Every `pre-edit` hook:

1. Reads the target `file_path` from the tool input.
2. Resolves it to a canonical absolute path.
3. Scans `$CLAUDE_SESSION_GUARD_HOME/state/*.json` for any other session with a claim on the same path within the last 10 minutes.
4. If a match exists, emits a `hookSpecificOutput.additionalContext` block the Claude Code runtime injects back into the LLM's context *before* the tool call runs.
5. Records its own claim so future siblings see it.

The LLM gets a message like:

> ⚠️ CONCURRENCY WARNING: 1 other Claude session(s) recently claimed `src/auth.ts`:
> `a1b2c3d4` in `backend-api` (cwd `/Users/me/code/backend-api`) claimed this 3m ago
>
> Consider pausing and coordinating before editing to avoid clobbering their work.

In practice Claude almost always pauses and surfaces the conflict. It's advisory — there's no hard lock. If you want it to proceed anyway, just tell it to.

## CLI

```text
claude-session-guard status            # show active sessions
claude-session-guard claim "intent"    # post a manual claim line
claude-session-guard release <file>    # drop a recorded claim
claude-session-guard gc                # reap sessions idle >24h
claude-session-guard test              # smoke-test Slack
```

## Data layout

Default: `$XDG_DATA_HOME/claude-session-guard/` (i.e. `~/.local/share/claude-session-guard/`).

Override with `CLAUDE_SESSION_GUARD_HOME=/some/path`.

```text
config.env              # SLACK_BOT_TOKEN, SLACK_CHANNEL_ID
events.log              # append-only operational log
state/<session_id>.json # one per live session, GC'd at 24h idle
```

State files are atomically written via `os.Rename()`. The binary re-invokes itself in `bg-post` mode for Slack I/O so the hook returns in milliseconds.

## Performance

- Cold start: ~5ms on an M3.
- Slack posts: off the hot path — fire-and-forget via detached child.
- PreToolUse latency cap: ~15ms including conflict scan over ~10 active sessions.

## Limitations

- Advisory only. The LLM can ignore the warning if you tell it to.
- Single machine. Multi-machine coordination would need a shared backend — see [notes below](#future-ideas).
- Slack free tier has a workspace message cap; on busy days you may hit it. The guard throttles to one post per file per 60s to mitigate.

## MCP server — ask Claude what the other sessions are doing

The binary ships a minimal MCP server. Wire it up and any Claude Code session can call `list_sessions` to see every other active session's current repo, branch, and last edited file — before it touches anything.

Add to your `~/.claude/mcp.json` (or project `.mcp.json`):

```json
{
  "mcpServers": {
    "claude-session-guard": {
      "command": "claude-session-guard",
      "args": ["mcp"]
    }
  }
}
```

Claude can then use the `list_sessions` tool. Example interaction:

**Claude:** I'll check what other sessions are doing before editing this file. `list_sessions →`

```text
2 active Claude Code session(s) on this machine:

Session 1: a1b2c3d4
  repo:    backend-api
  branch:  feat/auth-refactor
  cwd:     /Users/me/code/backend-api
  edits:   23
  last file: src/auth.ts (at 2026-04-18T14:32:01-04:00)

Session 2: ef901234
  repo:    backend-api
  branch:  feat/auth-refactor
  cwd:     /Users/me/code/backend-api
  edits:   7
  last file: tests/auth.test.ts (at 2026-04-18T14:31:44-04:00)
```

**Claude:** Another session is actively editing `src/auth.ts`. I'll check with you before making changes there.

The MCP server speaks JSON-RPC 2.0 over stdio — no network ports, no daemons.

## Future ideas

- Central Postgres sync so sessions on different laptops see each other (a SQLite-first, cluster-synced design is in progress — PRs welcome).
- Richer conflict policies: block instead of warn for specific paths, or prompt-for-confirmation patterns.

## Let Claude set this up for you

Paste the following prompt into any Claude Code session and it will install `claude-session-guard`, wire the hooks, and optionally configure Slack — no manual steps required.

```text
Set up claude-session-guard on this machine:

1. Clone https://github.com/zolty-mat/claude-session-guard to a temp dir,
   build with `make install` (installs binary to ~/.local/bin/), and confirm
   the binary is on PATH.

2. Open ~/.claude/settings.json (create if missing). Merge in the following
   hooks block — do NOT overwrite any existing hooks, merge carefully:
     SessionStart  → command: "claude-session-guard start"
     SessionEnd    → command: "claude-session-guard stop"
     PreToolUse matcher "Edit|Write|NotebookEdit" → command: "claude-session-guard pre-edit"

3. Run `claude-session-guard status` to confirm the binary works.

4. If I provide a SLACK_BOT_TOKEN and SLACK_CHANNEL_ID, write them to
   ~/.local/share/claude-session-guard/config.env with mode 0600,
   then run `claude-session-guard test` to verify Slack connectivity.
   If I don't provide tokens, skip this step — local-only mode is fine.

5. Report: binary path, settings.json hooks section (show me the merged
   result), and whether the status command returned cleanly.
```

## claude-session-guard as a Claude Code skill

If you use Claude Code skills (`.claude/skills/`), add this file as `claude-session-guard/SKILL.md` in your project to give Claude persistent context about the guard:

```markdown
# claude-session-guard

Parallel session coordinator. Binary at `~/.local/bin/claude-session-guard`.
State: `~/.local/share/claude-session-guard/state/`.

## Key commands
- `claude-session-guard status` — show all active sessions with edit counts
- `claude-session-guard claim "<intent>"` — post a manual intent marker to this session's Slack thread
- `claude-session-guard release <file>` — drop a recorded file claim
- `claude-session-guard gc` — reap sessions idle >24h
- `claude-session-guard test` — smoke-test Slack connectivity

## When to use
- Before starting work that might overlap with another session: run `status`
- When you see a ⚠️ CONCURRENCY WARNING in context: pause, check `status`, coordinate before editing
- After completing a major task: optionally run `claim "done with X"` so other sessions see the intent

## Conflict warnings
When pre-edit fires a conflict, you will see this in your context:
> ⚠️ CONCURRENCY WARNING: N other Claude session(s) recently claimed `<file>`:
> - `<session-id>` in `<repo>` claimed this Nm ago

Default response: surface the conflict to the user and ask before proceeding.
Only override if the user explicitly instructs you to proceed anyway.
```

## License

MIT. See [LICENSE](LICENSE).
