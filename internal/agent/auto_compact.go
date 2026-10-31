package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

// compactAt is the running token total at which a session on a model with
// context window cw compacts before its next request: as close to the
// window as leaves room for the reply (largeContextWindowBuffer of a large
// window, smallContextWindowRatio of a small one), or earlier where the
// user says, with Claude Code's variables or the auto_compact_window
// option /autocompact sets, window (0 for none, auto).
// CLAUDE_CODE_AUTO_COMPACT_WINDOW, which takes precedence over the option,
// and the option are how full the context may get, from 100k to 1M
// tokens, capped at the model's window; CLAUDE_AUTOCOMPACT_PCT_OVERRIDE is
// the percentage of that window at which to compact, and like it can only
// bring compaction earlier. Compacting earlier keeps a long session short,
// and with it the time a local model takes to read the conversation again
// when its cache was lost. Without overrides, neither is read: the
// default point.
func compactAt(cw, window int64, overrides bool) int64 {
	if cw <= 0 {
		cw = unknownContextWindowFallback
	}
	at := cw - int64(float64(cw)*smallContextWindowRatio)
	if cw > largeContextWindowThreshold {
		at = cw - largeContextWindowBuffer
	}
	if !overrides {
		return at
	}
	if w, ok := envAutoCompactWindow(); ok {
		window = w
	} else if window > 0 {
		window = optionAutoCompactWindow(window)
	}
	limit := cw
	if window > 0 {
		limit = min(window, cw)
		at = min(at, limit)
	}
	if p, ok := envAutoCompactPercent(); ok {
		at = min(at, limit*p/100)
	}
	return at
}

// optionAutoCompactWindow is the auto_compact_window option, raised to
// 100k or lowered to 1M as Claude Code takes its setting.
func optionAutoCompactWindow(window int64) int64 {
	switch {
	case window < 100_000:
		warnCompactVariable("auto_compact_window", strconv.FormatInt(window, 10), "it is under 100k tokens, so 100k is used")
		return 100_000
	case window > 1_000_000:
		warnCompactVariable("auto_compact_window", strconv.FormatInt(window, 10), "it is over 1M tokens, so 1M is used")
		return 1_000_000
	}
	return window
}

// autoCompactWindow is the auto_compact_window option as it is set now (0
// for auto): /autocompact changes it while the session runs.
func (a *sessionAgent) autoCompactWindow() int64 {
	if a.cfg == nil || a.cfg.Config().Options == nil {
		return 0
	}
	return a.cfg.Config().Options.AutoCompactWindow
}

// envAutoCompactWindow is CLAUDE_CODE_AUTO_COMPACT_WINDOW in tokens, as
// Claude Code reads it: a count, or one with a k or M suffix (200k, 1M),
// raised to 100k or lowered to 1M, and ignored when it is no number.
func envAutoCompactWindow() (int64, bool) {
	value := strings.TrimSpace(os.Getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW"))
	if value == "" {
		return 0, false
	}
	number, scale := value, int64(1)
	switch {
	case strings.HasSuffix(value, "k"), strings.HasSuffix(value, "K"):
		number, scale = value[:len(value)-1], 1_000
	case strings.HasSuffix(value, "m"), strings.HasSuffix(value, "M"):
		number, scale = value[:len(value)-1], 1_000_000
	}
	n, err := strconv.ParseFloat(number, 64)
	if err != nil || n <= 0 {
		warnCompactVariable("CLAUDE_CODE_AUTO_COMPACT_WINDOW", value, "it is not a token count, so it is ignored")
		return 0, false
	}
	tokens := int64(n * float64(scale))
	switch {
	case tokens < 100_000:
		warnCompactVariable("CLAUDE_CODE_AUTO_COMPACT_WINDOW", value, "it is under 100k tokens, so 100k is used")
		return 100_000, true
	case tokens > 1_000_000:
		warnCompactVariable("CLAUDE_CODE_AUTO_COMPACT_WINDOW", value, "it is over 1M tokens, so 1M is used")
		return 1_000_000, true
	}
	return tokens, true
}

// envAutoCompactPercent is CLAUDE_AUTOCOMPACT_PCT_OVERRIDE, a percentage
// from 1 to 100, or false when it is unset or anything else.
func envAutoCompactPercent() (int64, bool) {
	value := strings.TrimSpace(os.Getenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"))
	if value == "" {
		return 0, false
	}
	p, err := strconv.ParseInt(value, 10, 64)
	if err != nil || p < 1 || p > 100 {
		warnCompactVariable("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", value, "it is not a percentage from 1 to 100, so it is ignored")
		return 0, false
	}
	return p, true
}

// warnedCompactVariables are the values warnCompactVariable has warned
// about, so that each is logged once, not before every step.
var warnedCompactVariables sync.Map

func warnCompactVariable(name, value, why string) {
	if _, seen := warnedCompactVariables.LoadOrStore(name+"="+value, true); !seen {
		slog.Warn("An auto-compaction variable has a value Gnosh cannot use as given", "variable", name, "value", value, "why", why)
	}
}

// How far a session's auto-compaction goes; see sessionAgent.compacts.
const (
	compactAsAsked   = iota // at the point compactAt gives, overrides read
	compactAtDefault        // at the default point, overrides dropped
	compactNever            // not at all
)

// compacts reports whether a session whose running token total is tokens,
// on a model with context window cw, is summarized before its next step
// (see autoSummarize). A summary that leaves the conversation still over
// the point that set it off cannot bring it under, and summarizing again
// would only repeat it on every step, in a loop: then the session drops
// the earlier point the variables gave for the default, and if it is over
// that too, it stops compacting, until it is summarized by hand.
func (a *sessionAgent) compacts(sessionID string, cw, tokens int64) bool {
	window := a.autoCompactWindow()
	level, _ := a.compactLevels.Get(sessionID)
	if _, justSummarized := a.justSummarized.Take(sessionID); justSummarized {
		if level == compactAsAsked && autoSummarize(cw, window, tokens, a.disableAutoSummarize, true) {
			level = compactAtDefault
			slog.Warn("A summary left the conversation over the point at which it is compacted; compacting at the default point from now on",
				"session_id", sessionID, "tokens", tokens, "point", compactAt(cw, window, true))
		}
		if level == compactAtDefault && autoSummarize(cw, window, tokens, a.disableAutoSummarize, false) {
			level = compactNever
			slog.Warn("A summary left the conversation over the default point at which it is compacted; not compacting it again unless asked",
				"session_id", sessionID, "tokens", tokens, "point", compactAt(cw, window, false))
		}
		a.compactLevels.Set(sessionID, level)
	}
	return level != compactNever && autoSummarize(cw, window, tokens, a.disableAutoSummarize, level == compactAsAsked)
}

// slides is what a session standing in frames does at the point at which
// it would be compacted (see compacts): the start of its oldest open
// frame's work leaves the model's context instead (frameStacks.slide),
// since mem holds that frame's plan, its results and its thoughts, and the
// summary is kept for a session with no frame to cut. The slide frees what
// is over the point and a tenth of the window besides, so the rounds that
// follow have room. It answers the line that tells the model what left,
// and whether a slide was made.
func (a *sessionAgent) slides(ctx context.Context, sessionID string, cw, tokens int64) (string, bool) {
	level, _ := a.compactLevels.Get(sessionID)
	window := cw
	if window <= 0 {
		window = unknownContextWindowFallback
	}
	need := tokens - compactAt(cw, a.autoCompactWindow(), level == compactAsAsked) + window/10
	line, _, ok := a.frames.slide(ctx, sessionID, need)
	return line, ok
}

// compactsOrSlides is what a session over the point at which it is
// compacted (see compacts) does: standing in frames it slides when it can
// (see slides), else it is summarized. compact says the latter; slid is
// the line that tells the model of a slide.
func (a *sessionAgent) compactsOrSlides(ctx context.Context, sessionID string, cw, tokens int64) (compact bool, slid string) {
	if !a.compacts(sessionID, cw, tokens) {
		return false, ""
	}
	line, ok := a.slides(ctx, sessionID, cw, tokens)
	return !ok, line
}

// compactsNow is compactsOrSlides before a prompt goes out, where the
// prompt's frame line tells the model of a slide (frameStacks.stackLine).
func (a *sessionAgent) compactsNow(ctx context.Context, sessionID string, cw, tokens int64) bool {
	compact, _ := a.compactsOrSlides(ctx, sessionID, cw, tokens)
	return compact
}

// compactBeforeRequest summarizes the session's conversation before the
// turn of call sends its prompt, as that turn's first work: in its
// context, genCtx, so Esc stops it, and with the prompt kept out of it, to
// follow the summary. Its PreCompact and PostCompact hooks run as for any
// automatic summary, and the prompt, the first after it, runs the
// SessionStart hooks with source "compact" and fires the
// InstructionsLoaded hooks again. A summary that fails leaves the
// conversation as it was, and the prompt goes out whole, as Claude Code
// goes on when compacting fails. One cancelled ends the turn:
// context.Canceled.
func (a *sessionAgent) compactBeforeRequest(ctx, genCtx context.Context, call *SessionAgentCall) error {
	slog.Info("Compacting the conversation before sending the prompt", "session_id", call.SessionID)
	a.runPreCompactHooks(genCtx, call.SessionID, preCompactTriggerAuto, "")
	err := a.compactConversation(ctx, genCtx, call.SessionID, call.Channel, "", call.ProviderOptions, call.OnAuthRefresh, preCompactTriggerAuto)
	if errors.Is(err, errSummaryCanceled) || genCtx.Err() != nil {
		return context.Canceled
	}
	if err != nil {
		slog.Warn("Could not compact the conversation before sending the prompt; sending it whole", "session_id", call.SessionID, "error", err)
		return nil
	}
	a.justSummarized.Set(call.SessionID, true)
	// Under the submit lock, as at the start of Run, so a prompt sent
	// meanwhile does not run them a second time.
	submitMu := a.sessionSubmitMu(call.SessionID)
	submitMu.Lock()
	if a.wantsSessionStartHooks(*call) && a.runSessionStartHooks(genCtx, call) && genCtx.Err() == nil {
		a.sessionStarts.markStarted(call.SessionID)
	}
	submitMu.Unlock()
	a.fireInstructionsLoadedForSession(call.SessionID)
	return nil
}

// idleCompactionAfter is how long a session waits idle before its
// conversation is compacted, as in Claude Code: about 54 minutes, shortly
// before an hour-long prompt cache expires.
const idleCompactionAfter = 54 * time.Minute

// armIdleCompaction starts, when a turn ends in an interactive session,
// the wait after which a long conversation left idle is summarized, as
// Claude Code's idle compaction does (see compactIdle), in place of the
// wait an earlier turn started. A wait that runs out while a turn runs
// does nothing (compactIdle finds the session busy), and the turn's end
// starts another. Not for a sub-agent, or with auto-summarize or idle
// compaction turned off.
func (a *sessionAgent) armIdleCompaction(sessionID string, opts fantasy.ProviderOptions) {
	if a.isSubAgent || a.disableAutoSummarize || a.disableIdleCompaction || a.idleCompactAfter <= 0 {
		return
	}
	a.disarmIdleCompaction(sessionID)
	a.idleCompaction.Set(sessionID, time.AfterFunc(a.idleCompactAfter, func() { a.compactIdle(sessionID, opts) }))
}

// disarmIdleCompaction stops the session's idle-compaction wait.
func (a *sessionAgent) disarmIdleCompaction(sessionID string) {
	if timer, ok := a.idleCompaction.Take(sessionID); ok {
		timer.Stop()
	}
}

// compactIdle summarizes a session left idle when its conversation has
// reached idleCompactMinTokens, so the next prompt sends a summary rather
// than the whole conversation. A local model whose cache another
// conversation took meanwhile reads it all again before it answers; here
// it does so while no one waits, and while the cache may still hold it. A
// turn running in the session stops the summary (summarize). A prompt
// sent while it runs does not wait for it: the summary is yieldable, so
// Run gives it up and the prompt goes first (see activeCancel).
func (a *sessionAgent) compactIdle(sessionID string, opts fantasy.ProviderOptions) {
	ctx := context.Background()
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return
	}
	tokens := sess.PromptTokens + sess.CompletionTokens
	if tokens < idleCompactMinTokens() {
		return
	}
	slog.Info("Compacting a conversation left idle", "session_id", sessionID, "tokens", tokens)
	// A summary the user's prompt pushed aside reports nothing to say:
	// summarize drops its half-written summary message and returns nil
	// when it is cancelled. Run logs the hand-over where it happens.
	if err := a.summarize(ctx, sessionID, "", "", opts, nil, preCompactTriggerAuto, true); err != nil && !errors.Is(err, ErrSessionBusy) {
		slog.Warn("Could not compact a conversation left idle", "session_id", sessionID, "error", err)
	}
}

// idleCompactMinTokens is how large a conversation must be for idle
// compaction: CLAUDE_CODE_IDLE_COMPACT_MIN_TOKENS, as in Claude Code, a
// positive whole number raised to 100000 when under it, else 200000.
func idleCompactMinTokens() int64 {
	value := strings.TrimSpace(os.Getenv("CLAUDE_CODE_IDLE_COMPACT_MIN_TOKENS"))
	if value == "" {
		return 200_000
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		warnCompactVariable("CLAUDE_CODE_IDLE_COMPACT_MIN_TOKENS", value, "it is not a positive whole number, so it is ignored")
		return 200_000
	}
	return max(n, 100_000)
}

// readRoom is how many tokens view's allow_large may bring into a
// conversation of tokens, on a model with context window cw, in one step:
// as Claude Code allows, 70% of what is left before the point at which the
// conversation is compacted (see compactAt; window is the
// auto_compact_window option), or before the window's end when
// compaction is off.
func readRoom(cw, window, tokens int64, disabled bool) int {
	end := compactAt(cw, window, true)
	if disabled {
		end = cw
	}
	return int(float64(end-tokens) * 0.7)
}
