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

1. Create a Slack app at https://api.slack.com/apps → "From scratch."
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
> - `a1b2c3d4` in `backend-api` (cwd `/Users/me/code/backend-api`) claimed this 3m ago
>
> Consider pausing and coordinating before editing to avoid clobbering their work.

In practice Claude almost always pauses and surfaces the conflict. It's advisory — there's no hard lock. If you want it to proceed anyway, just tell it to.

## CLI

```
claude-session-guard status            # show active sessions
claude-session-guard claim "intent"    # post a manual claim line
claude-session-guard release <file>    # drop a recorded claim
claude-session-guard gc                # reap sessions idle >24h
claude-session-guard test              # smoke-test Slack
```

## Data layout

Default: `$XDG_DATA_HOME/claude-session-guard/` (i.e. `~/.local/share/claude-session-guard/`).

Override with `CLAUDE_SESSION_GUARD_HOME=/some/path`.

```
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

## Future ideas

- Central Postgres sync so sessions on different laptops see each other (a SQLite-first, cluster-synced design is in progress — PRs welcome).
- `MCP` server exposing active sessions as a resource so one Claude can "see" what the others are doing.
- Richer conflict policies: block instead of warn for specific paths, or prompt-for-confirmation patterns.

## License

MIT. See [LICENSE](LICENSE).
