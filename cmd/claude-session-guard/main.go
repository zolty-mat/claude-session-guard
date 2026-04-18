// claude-session-guard: advisory file-level coordination for parallel Claude Code sessions.
//
// Plugs into Claude Code hooks (SessionStart, SessionEnd, PreToolUse) to
// track which sessions are editing which files, warn on near-simultaneous
// edits, and (optionally) post a live timeline to a Slack thread.
//
// State lives on local disk as JSON. Slack is opt-in — set SLACK_BOT_TOKEN
// and SLACK_CHANNEL_ID to enable, omit for a local-only coordinator.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	conflictWindowMin = 10
	claimHistoryCap   = 50
	staleSessionHours = 24
	httpTimeout       = 5 * time.Second
	notifyThrottle    = 60 * time.Second
)

var (
	homeDir    string
	configFile string
	stateDir   string
	logFile    string
	binPath    string
	httpClient = &http.Client{Timeout: httpTimeout}
)

func init() {
	base := os.Getenv("CLAUDE_SESSION_GUARD_HOME")
	if base == "" {
		u, err := user.Current()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot resolve home dir:", err)
			os.Exit(1)
		}
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg == "" {
			xdg = filepath.Join(u.HomeDir, ".local", "share")
		}
		base = filepath.Join(xdg, "claude-session-guard")
	}
	homeDir = base
	configFile = filepath.Join(base, "config.env")
	stateDir = filepath.Join(base, "state")
	logFile = filepath.Join(base, "events.log")
	if v := os.Getenv("CLAUDE_SESSION_GUARD_CONFIG"); v != "" {
		configFile = v
	}
	exe, err := os.Executable()
	if err == nil {
		binPath, _ = filepath.EvalSymlinks(exe)
	}
}

// ─── utilities ──────────────────────────────────────────────────────────────

func logMsg(format string, args ...any) {
	_ = os.MkdirAll(filepath.Dir(logFile), 0o755)
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02T15:04:05"), fmt.Sprintf(format, args...))
}

// loadConfig reads KEY=VALUE pairs from configFile if present. Environment
// variables take precedence — config file is a fallback for users who don't
// want to export secrets in their shell.
func loadConfig() map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(configFile)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			eq := strings.Index(line, "=")
			if eq < 0 {
				continue
			}
			k := strings.TrimSpace(line[:eq])
			v := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
			out[k] = v
		}
	}
	for _, k := range []string{"SLACK_BOT_TOKEN", "SLACK_CHANNEL_ID"} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func nowLocalISO() string { return time.Now().Format(time.RFC3339) }
func fmtTime() string     { return time.Now().Format("3:04 PM MST") }

func shortID(sid string) string {
	if len(sid) >= 8 {
		return sid[:8]
	}
	if sid == "" {
		return "nosid"
	}
	return sid
}

func gitBranch(cwd string) string {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func relPath(file, base string) string {
	r, err := filepath.Rel(base, file)
	if err != nil || strings.HasPrefix(r, "..") {
		return file
	}
	return r
}

func canonPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

func hostname() string {
	h, _ := os.Hostname()
	if i := strings.Index(h, "."); i >= 0 {
		return h[:i]
	}
	return h
}

// ─── slack client ──────────────────────────────────────────────────────────

func slackAPI(method string, payload map[string]any, token string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", "https://slack.com/api/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := httpClient.Do(req)
	if err != nil {
		logMsg("slack %s exception: %v", method, err)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		logMsg("slack %s parse: %v", method, err)
		return nil, err
	}
	if ok, _ := out["ok"].(bool); !ok {
		logMsg("slack %s error: %s", method, string(raw))
	}
	return out, nil
}

type slackAction struct {
	Method  string         `json:"method"`
	Payload map[string]any `json:"payload"`
}

// runDetached spawns this binary in `bg-post` mode so Slack I/O doesn't block
// the hook. Payload is handed off via temp file — a stdin pipe races with
// parent exit and the child ends up with empty stdin.
func runDetached(actions []slackAction, token string) {
	if len(actions) == 0 || token == "" || binPath == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"actions": actions, "token": token})
	if err != nil {
		logMsg("detach marshal: %v", err)
		return
	}
	inline := func() {
		for _, a := range actions {
			_, _ = slackAPI(a.Method, a.Payload, token)
		}
	}
	tmp, err := os.CreateTemp("", "csg-bg-*.json")
	if err != nil {
		logMsg("detach tempfile: %v; running inline", err)
		inline()
		return
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		logMsg("detach write: %v; running inline", err)
		inline()
		return
	}
	tmp.Close()
	cmd := exec.Command(binPath, "bg-post", tmp.Name())
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = newSysProcAttr()
	if err := cmd.Start(); err != nil {
		os.Remove(tmp.Name())
		logMsg("detach start: %v; running inline", err)
		inline()
		return
	}
	_ = cmd.Process.Release()
}

// ─── session state ─────────────────────────────────────────────────────────

type Claim struct {
	File string `json:"file"`
	Abs  string `json:"abs,omitempty"`
	At   string `json:"at"`
}

type State struct {
	SessionID    string  `json:"session_id"`
	SessionShort string  `json:"session_short"`
	Channel      string  `json:"channel,omitempty"`
	ThreadTS     string  `json:"thread_ts,omitempty"`
	CWD          string  `json:"cwd"`
	Repo         string  `json:"repo"`
	Branch       string  `json:"branch"`
	Host         string  `json:"host"`
	StartedAt    string  `json:"started_at"`
	Claims       []Claim `json:"claims"`
	EditCount    int     `json:"edit_count"`
	Lazy         bool    `json:"lazy,omitempty"`
}

func statePath(sid string) string { return filepath.Join(stateDir, sid+".json") }

func loadState(sid string) *State {
	data, err := os.ReadFile(statePath(sid))
	if err != nil {
		return nil
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return &s
}

func saveState(s *State) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	final := statePath(s.SessionID)
	tmp := fmt.Sprintf("%s.%d.tmp", final, os.Getpid())
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func allStates() []*State {
	var out []*State
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			continue
		}
		var s State
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		out = append(out, &s)
	}
	return out
}

func findSessionForCWD(cwd string) *State {
	var matches []*State
	for _, s := range allStates() {
		if s.CWD == cwd {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].StartedAt > matches[j].StartedAt })
	return matches[0]
}

func latestActivity(s *State) string {
	latest := s.StartedAt
	for _, c := range s.Claims {
		if c.At > latest {
			latest = c.At
		}
	}
	return latest
}

func gcStaleSessions(cfg map[string]string) {
	token := cfg["SLACK_BOT_TOKEN"]
	now := time.Now()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(stateDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		var s State
		if err := json.Unmarshal(data, &s); err != nil {
			_ = os.Remove(path)
			continue
		}
		latest, err := time.Parse(time.RFC3339, latestActivity(&s))
		ageHr := 999.0
		if err == nil {
			ageHr = now.Sub(latest).Hours()
		}
		if ageHr < staleSessionHours {
			continue
		}
		if token != "" && s.Channel != "" && s.ThreadTS != "" {
			_, _ = slackAPI("chat.postMessage", map[string]any{
				"channel":   s.Channel,
				"thread_ts": s.ThreadTS,
				"text":      fmt.Sprintf("🪦 presumed crashed — no activity for %dh, state GC'd", int(ageHr)),
			}, token)
			_, _ = slackAPI("reactions.remove", map[string]any{"channel": s.Channel, "timestamp": s.ThreadTS, "name": "large_green_circle"}, token)
			_, _ = slackAPI("reactions.add", map[string]any{"channel": s.Channel, "timestamp": s.ThreadTS, "name": "headstone"}, token)
		}
		_ = os.Remove(path)
	}
}

// ─── hook handlers ─────────────────────────────────────────────────────────

type hookInput struct {
	SessionID string         `json:"session_id"`
	CWD       string         `json:"cwd"`
	Source    string         `json:"source,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
}

func readHookInput() hookInput {
	var in hookInput
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return in
	}
	_ = json.Unmarshal(data, &in)
	return in
}

func openThread(sid, cwd string, cfg map[string]string, lazy bool) *State {
	branch := gitBranch(cwd)
	if branch == "" {
		branch = "—"
	}
	repo := filepath.Base(cwd)
	if repo == "" {
		repo = cwd
	}
	host := hostname()
	short := shortID(sid)

	state := &State{
		SessionID:    sid,
		SessionShort: short,
		CWD:          cwd,
		Repo:         repo,
		Branch:       branch,
		Host:         host,
		StartedAt:    nowLocalISO(),
		Claims:       []Claim{},
		EditCount:    0,
		Lazy:         lazy,
	}

	token := cfg["SLACK_BOT_TOKEN"]
	channel := cfg["SLACK_CHANNEL_ID"]
	if token != "" && channel != "" {
		gcStaleSessions(cfg)
		emoji := "🟢"
		title := "session started"
		if lazy {
			emoji = "🟡"
			title = "session attached mid-flight"
		}
		resp, err := slackAPI("chat.postMessage", map[string]any{
			"channel": channel,
			"text":    fmt.Sprintf("%s %s — `%s` in `%s`", emoji, title, short, repo),
			"blocks": []map[string]any{
				{"type": "section", "text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("%s *%s* — `%s`", emoji, title, short),
				}},
				{"type": "section", "fields": []map[string]any{
					{"type": "mrkdwn", "text": fmt.Sprintf("*repo*\n`%s`", repo)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*branch*\n`%s`", branch)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*cwd*\n`%s`", cwd)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*host*\n`%s`", host)},
					{"type": "mrkdwn", "text": fmt.Sprintf("*started*\n%s", fmtTime())},
					{"type": "mrkdwn", "text": fmt.Sprintf("*session*\n`%s`", short)},
				}},
			},
		}, token)
		if err == nil && resp != nil {
			if ok, _ := resp["ok"].(bool); ok {
				state.Channel = channel
				state.ThreadTS, _ = resp["ts"].(string)
				_, _ = slackAPI("reactions.add", map[string]any{"channel": channel, "timestamp": state.ThreadTS, "name": "large_green_circle"}, token)
			}
		}
	}

	if err := saveState(state); err != nil {
		logMsg("save state: %v", err)
	}
	return state
}

func ensureState(sid, cwd string, cfg map[string]string) *State {
	if s := loadState(sid); s != nil {
		return s
	}
	return openThread(sid, cwd, cfg, true)
}

func handleStart() {
	in := readHookInput()
	cfg := loadConfig()
	sid := in.SessionID
	if sid == "" {
		sid = "nosid"
	}
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	source := in.Source
	if source == "" {
		source = "startup"
	}
	if source == "clear" || source == "compact" {
		if existing := loadState(sid); existing != nil {
			token := cfg["SLACK_BOT_TOKEN"]
			if token != "" && existing.Channel != "" && existing.ThreadTS != "" {
				runDetached([]slackAction{{
					Method: "chat.postMessage",
					Payload: map[string]any{
						"channel":   existing.Channel,
						"thread_ts": existing.ThreadTS,
						"text":      fmt.Sprintf("↩️ context %s", source),
					},
				}}, token)
			}
		}
		return
	}
	if loadState(sid) != nil {
		return
	}
	openThread(sid, cwd, cfg, false)
}

func handleStop() {
	in := readHookInput()
	cfg := loadConfig()
	sid := in.SessionID
	state := loadState(sid)
	if state == nil {
		return
	}
	reason := in.Reason
	if reason == "" {
		reason = "ended"
	}
	dur := "?"
	if started, err := time.Parse(time.RFC3339, state.StartedAt); err == nil {
		secs := int(time.Since(started).Seconds())
		switch {
		case secs < 60:
			dur = fmt.Sprintf("%ds", secs)
		case secs < 3600:
			dur = fmt.Sprintf("%dm", secs/60)
		default:
			dur = fmt.Sprintf("%dh %dm", secs/3600, (secs%3600)/60)
		}
	}
	closing := fmt.Sprintf("✅ session %s — %s · %d edits · %d claims",
		reason, dur, state.EditCount, len(state.Claims))
	_ = os.Remove(statePath(sid))

	token := cfg["SLACK_BOT_TOKEN"]
	if token == "" || state.Channel == "" || state.ThreadTS == "" {
		return
	}
	runDetached([]slackAction{
		{Method: "chat.postMessage", Payload: map[string]any{"channel": state.Channel, "thread_ts": state.ThreadTS, "text": closing}},
		{Method: "reactions.remove", Payload: map[string]any{"channel": state.Channel, "timestamp": state.ThreadTS, "name": "large_green_circle"}},
		{Method: "reactions.add", Payload: map[string]any{"channel": state.Channel, "timestamp": state.ThreadTS, "name": "white_check_mark"}},
	}, token)
}

func handlePreEdit() {
	in := readHookInput()
	cfg := loadConfig()
	if in.ToolName != "Edit" && in.ToolName != "Write" && in.ToolName != "NotebookEdit" {
		return
	}
	sid := in.SessionID
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	filePath, _ := in.ToolInput["file_path"].(string)
	if filePath == "" {
		filePath = "unknown"
	}
	state := ensureState(sid, cwd, cfg)
	if state == nil {
		return
	}
	absFile := canonPath(filePath)
	pretty := relPath(filePath, state.CWD)

	type conflictInfo struct {
		Other *State
		Age   int
	}
	var conflicts []conflictInfo
	now := time.Now()
	for _, other := range allStates() {
		if other.SessionID == sid {
			continue
		}
		for _, c := range other.Claims {
			match := false
			if c.Abs != "" {
				match = c.Abs == absFile
			} else {
				match = c.File == pretty || c.File == filePath
			}
			if !match {
				continue
			}
			at, err := time.Parse(time.RFC3339, c.At)
			if err != nil {
				continue
			}
			age := int(now.Sub(at).Minutes())
			if age <= conflictWindowMin {
				conflicts = append(conflicts, conflictInfo{Other: other, Age: age})
			}
		}
	}

	lastNotified := time.Time{}
	for _, c := range state.Claims {
		if c.Abs == absFile || c.File == pretty || c.File == filePath {
			if t, err := time.Parse(time.RFC3339, c.At); err == nil && t.After(lastNotified) {
				lastNotified = t
			}
		}
	}
	shouldPost := time.Since(lastNotified) >= notifyThrottle

	state.Claims = append(state.Claims, Claim{File: pretty, Abs: absFile, At: nowLocalISO()})
	if len(state.Claims) > claimHistoryCap {
		state.Claims = state.Claims[len(state.Claims)-claimHistoryCap:]
	}
	state.EditCount++
	_ = saveState(state)

	if len(conflicts) > 0 {
		var lines []string
		for _, c := range conflicts {
			lines = append(lines, fmt.Sprintf("- `%s` in `%s` (cwd `%s`) claimed this %dm ago",
				shortID(c.Other.SessionID), c.Other.Repo, c.Other.CWD, c.Age))
		}
		ctx := fmt.Sprintf("⚠️ CONCURRENCY WARNING: %d other Claude session(s) recently claimed `%s`:\n%s\n\nConsider pausing and coordinating before editing to avoid clobbering their work.",
			len(conflicts), pretty, strings.Join(lines, "\n"))
		out, _ := json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PreToolUse",
				"additionalContext": ctx,
			},
		})
		fmt.Println(string(out))
		os.Stdout.Sync()
	}

	token := cfg["SLACK_BOT_TOKEN"]
	if token == "" || state.Channel == "" || state.ThreadTS == "" {
		return
	}
	if !shouldPost && len(conflicts) == 0 {
		return
	}

	var actions []slackAction
	ownText := fmt.Sprintf("✏️ editing `%s`", pretty)
	if len(conflicts) > 0 {
		ownText = fmt.Sprintf("⚠️ editing `%s` — conflict with %d other session(s)", pretty, len(conflicts))
	}
	actions = append(actions, slackAction{
		Method:  "chat.postMessage",
		Payload: map[string]any{"channel": state.Channel, "thread_ts": state.ThreadTS, "text": ownText},
	})
	for _, c := range conflicts {
		if c.Other.Channel == "" || c.Other.ThreadTS == "" {
			continue
		}
		actions = append(actions, slackAction{
			Method: "chat.postMessage",
			Payload: map[string]any{
				"channel":   c.Other.Channel,
				"thread_ts": c.Other.ThreadTS,
				"text": fmt.Sprintf("⚠️ session `%s` (`%s`) is about to edit `%s` which you claimed %dm ago",
					shortID(sid), state.Repo, pretty, c.Age),
			},
		})
	}
	runDetached(actions, token)
}

// ─── CLI helpers ───────────────────────────────────────────────────────────

func handleClaim(args []string) {
	intent := strings.TrimSpace(strings.Join(args, " "))
	if intent == "" {
		intent = "(no intent given)"
	}
	cfg := loadConfig()
	sid := os.Getenv("CLAUDE_SESSION_ID")
	var state *State
	if sid != "" {
		state = loadState(sid)
	}
	if state == nil {
		cwd, _ := os.Getwd()
		state = findSessionForCWD(cwd)
	}
	if state == nil {
		fmt.Fprintln(os.Stderr, "no active session for this cwd")
		os.Exit(1)
	}
	token := cfg["SLACK_BOT_TOKEN"]
	if token != "" && state.Channel != "" && state.ThreadTS != "" {
		_, _ = slackAPI("chat.postMessage", map[string]any{
			"channel":   state.Channel,
			"thread_ts": state.ThreadTS,
			"text":      "📌 claim: " + intent,
		}, token)
	}
	fmt.Printf("posted to %s (%s)\n", state.SessionShort, state.Repo)
}

func handleRelease(args []string) {
	target := ""
	if len(args) > 0 {
		target = strings.TrimSpace(args[0])
	}
	cfg := loadConfig()
	sid := os.Getenv("CLAUDE_SESSION_ID")
	var state *State
	if sid != "" {
		state = loadState(sid)
	}
	if state == nil {
		cwd, _ := os.Getwd()
		state = findSessionForCWD(cwd)
	}
	if state == nil {
		fmt.Fprintln(os.Stderr, "no active session")
		os.Exit(1)
	}
	targetAbs := ""
	if target != "" {
		targetAbs = canonPath(target)
	}
	var kept []Claim
	for _, c := range state.Claims {
		if c.File == target || c.Abs == targetAbs {
			continue
		}
		kept = append(kept, c)
	}
	state.Claims = kept
	_ = saveState(state)
	token := cfg["SLACK_BOT_TOKEN"]
	if token != "" && state.Channel != "" && state.ThreadTS != "" {
		_, _ = slackAPI("chat.postMessage", map[string]any{
			"channel":   state.Channel,
			"thread_ts": state.ThreadTS,
			"text":      fmt.Sprintf("🔓 released claim on `%s`", target),
		}, token)
	}
	fmt.Printf("released on %s\n", state.SessionShort)
}

func handleStatus() {
	sessions := allStates()
	if len(sessions) == 0 {
		fmt.Println("no active sessions")
		return
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].StartedAt < sessions[j].StartedAt })
	fmt.Printf("%-10s %-24s %-16s %-20s EDITS  CWD\n", "ID", "REPO", "BRANCH", "STARTED")
	for _, s := range sessions {
		started := s.StartedAt
		if len(started) > 19 {
			started = started[:19]
		}
		repo := s.Repo
		if len(repo) > 24 {
			repo = repo[:24]
		}
		branch := s.Branch
		if len(branch) > 16 {
			branch = branch[:16]
		}
		fmt.Printf("%-10s %-24s %-16s %-20s %-6d %s\n",
			s.SessionShort, repo, branch, started, s.EditCount, s.CWD)
	}
}

func handleGC() {
	gcStaleSessions(loadConfig())
	fmt.Println("gc done")
}

func handleTest() {
	cfg := loadConfig()
	token := cfg["SLACK_BOT_TOKEN"]
	channel := cfg["SLACK_CHANNEL_ID"]
	if token == "" || channel == "" {
		fmt.Fprintln(os.Stderr, "missing SLACK_BOT_TOKEN or SLACK_CHANNEL_ID (set in env or", configFile+")")
		os.Exit(1)
	}
	resp, err := slackAPI("chat.postMessage", map[string]any{
		"channel": channel,
		"text":    fmt.Sprintf("🧪 hook test from `%s` at %s — claude-session-guard verified.", hostname(), fmtTime()),
	}, token)
	if err != nil {
		fmt.Println("❌", err)
		os.Exit(1)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		fmt.Printf("❌ failed: %v\n", resp)
		os.Exit(1)
	}
	ts, _ := resp["ts"].(string)
	fmt.Printf("✅ posted test message (ts=%s)\n", ts)
}

func handleBgPost() {
	var data []byte
	var err error
	if len(os.Args) > 2 {
		fname := os.Args[2]
		data, err = os.ReadFile(fname)
		_ = os.Remove(fname)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		logMsg("bg-post read: %v", err)
		return
	}
	var body struct {
		Token   string        `json:"token"`
		Actions []slackAction `json:"actions"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		logMsg("bg-post parse: %v", err)
		return
	}
	if body.Token == "" {
		return
	}
	for _, a := range body.Actions {
		_, _ = slackAPI(a.Method, a.Payload, body.Token)
	}
}

// ─── entrypoint ────────────────────────────────────────────────────────────

func usage() {
	fmt.Fprintln(os.Stderr, `usage: claude-session-guard <command>

Hook commands (wired into Claude Code via settings):
  start      SessionStart hook
  stop       SessionEnd hook
  pre-edit   PreToolUse hook (Edit|Write|NotebookEdit)

Operator commands:
  status     show active sessions and claim counts
  claim <intent>   post a manual claim to the active session's Slack thread
  release <file>   release a recorded claim
  gc         reap stale sessions (no activity for 24h)
  test       post a smoke-test message to the configured Slack channel

Internal:
  bg-post    background worker (do not invoke directly)

Config precedence: environment > $CLAUDE_SESSION_GUARD_CONFIG file > default config.env
Data dir default: $XDG_DATA_HOME/claude-session-guard (override with $CLAUDE_SESSION_GUARD_HOME)`)
	os.Exit(1)
}

func main() {
	_ = runtime.GOOS
	if len(os.Args) < 2 {
		usage()
	}
	cmd, rest := os.Args[1], os.Args[2:]
	defer func() {
		if r := recover(); r != nil {
			logMsg("panic in %s: %v", cmd, r)
		}
	}()
	switch cmd {
	case "start":
		handleStart()
	case "stop":
		handleStop()
	case "pre-edit":
		handlePreEdit()
	case "claim":
		handleClaim(rest)
	case "release":
		handleRelease(rest)
	case "status":
		handleStatus()
	case "gc":
		handleGC()
	case "test":
		handleTest()
	case "bg-post":
		handleBgPost()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", cmd)
		os.Exit(1)
	}
}
