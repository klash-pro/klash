// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A session's /goal is a goal in mem that the session is bound to (MEMPLAN
// §7). mem keeps the goal, its steps and what came of them; gnosh keeps the
// binding with the session, and as each of the session's turns ends it
// reads the goal and says what comes next: the agent goes on, the turn
// ends, or the judge is asked whether the goal, closed as done, holds.

// GoalBinding is a session's binding to a goal in its project's mem.
type GoalBinding struct {
	// Condition is what the user typed after /goal.
	Condition string `json:"condition"`
	// Instance is the id of the mem the goal is in; a mem made anew has
	// another.
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	// Added says that binding added the goal, which unbinding then drops.
	Added bool `json:"added,omitempty"`
	// Checks is how many times the judge was asked, and Verdict what it
	// last said.
	Checks  int    `json:"checks,omitempty"`
	Verdict string `json:"verdict,omitempty"`
	// Asked is what held the goal back when a turn last ended on the
	// checkpoint question: the same at the next turn's end ends the turn.
	Asked string `json:"asked,omitempty"`
}

// maxGoalTitle is the most of a condition's first line a goal's title
// takes, cut at a word: within mem's own cap, so the same condition always
// makes the same title, and so binds the same goal.
const maxGoalTitle = 200

// goalTitle is the title /goal gives the goal for condition.
func goalTitle(condition string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(condition), "\n")
	line = strings.Join(strings.Fields(line), " ")
	if len(line) <= maxGoalTitle {
		return line
	}
	cut := strings.LastIndexByte(line[:maxGoalTitle+1], ' ')
	if cut <= 0 {
		cut = maxGoalTitle
		for cut > 0 && !utf8RuneStart(line[cut]) {
			cut--
		}
	}
	return strings.TrimSpace(line[:cut])
}

// GoalDirective is what /goal sends the agent once its goal is bound.
func GoalDirective(b GoalBinding) string {
	return fmt.Sprintf("Goal `%[1]s` %[2]q is set: %[3]s\n\n"+
		"Plan it as todos (`mem.Todo(\"…\")` for each, in order) and work through them in frames: you stand in the goal already, so "+
		"`mem.Next()` steps into its first todo, and `mem.Pop(\"what came of it\")` then `mem.Next()` on the next line moves on. "+
		"Keep as facts what future sessions "+
		"should know, as you learn it. Once the goal holds, close it with `mem.Done(%[1]q, \"<command> → <result>\")`; "+
		"a separate check confirms it then. If the condition says when to stop and that has happened, drop the steps "+
		"left (`mem.Drop(name, \"why\")`), then close it with `mem.Done(%[1]q, \"stopped: <why>\")`.",
		b.Name, b.Title, strings.TrimSpace(b.Condition))
}

// bindGoal binds session to the goal for condition, as one input by the
// user (Mem.Bind): the open goal titled as the condition's first line, or
// else a goal it adds. prev is the session's earlier binding, if any: the
// goal it added, if it did, is dropped as replaced, unless it is the one
// bound now.
func (h *memHost) bindGoal(ctx context.Context, session, condition string, prev *GoalBinding) (GoalBinding, error) {
	title := goalTitle(condition)
	if title == "" {
		return GoalBinding{}, &refusal{"/goal takes what should hold: /goal go test ./... passes"}
	}
	args := strconv.Quote(title)
	if prev != nil && prev.Added && prev.Name != "" {
		args += ", " + strconv.Quote(prev.Name)
	}
	res, err := h.call(ctx, memRequest{Code: "print(mem.JSON(mem.Bind(" + args + ").Id))", Session: session, User: true})
	if err != nil {
		return GoalBinding{}, err
	}
	g, err := lastGoalJSON(res.Output)
	if err != nil {
		return GoalBinding{}, err
	}
	b := GoalBinding{Condition: strings.TrimSpace(condition), Instance: res.Instance, Name: g.Name, Title: g.Title}
	// The goal the earlier binding added, bound again, is still one /goal added.
	b.Added = prev != nil && prev.Added && prev.Name == b.Name && prev.Instance == b.Instance
	for _, op := range res.Ops {
		b.Added = b.Added || op.Kind == "add" && op.Name == g.Name
	}
	return b, nil
}

// memGoalJSON is a goal as mem.JSON(name) gives it.
type memGoalJSON struct {
	MemGoal
	NextTitle string `json:"next_title"`
	Result    string `json:"result"`
	Wait      string `json:"wait"`
	Asks      []struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	} `json:"asks"`
	Held []memHeld `json:"held"`
}

// lastGoalJSON reads the goal that mem.JSON(name), printed last, gives.
func lastGoalJSON(out string) (memGoalJSON, error) {
	out = strings.TrimRight(out, "\n")
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	var j struct {
		Goals []memGoalJSON `json:"goals"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil || len(j.Goals) != 1 {
		return memGoalJSON{}, fmt.Errorf("mem's answer about a goal could not be read: %q", memLine(out, 200))
	}
	return j.Goals[0], nil
}

// GoalTurn is what gnosh does with a session's goal as a turn ends.
type GoalTurn struct {
	// Do is one of:
	//   - "continue": send Prompt, hidden, and go on;
	//   - "checkpoint": send Prompt, which asks whether there is a real
	//     reason to stop, and go on;
	//   - "end": end the turn, the goal still bound; Notice says why;
	//   - "judge": the goal is closed as done: ask the judge whether the
	//     condition holds, showing it Show;
	//   - "rebind": mem was made anew; bind the condition again;
	//   - "clear": unbind; Notice says why.
	Do     string `json:"do"`
	Prompt string `json:"prompt,omitempty"`
	Notice string `json:"notice,omitempty"`
	// Show is mem.Show(name): the goal's steps, results and notes, the
	// agent's own claims, for the judge.
	Show string `json:"show,omitempty"`
}

// goalTurnEnd reads session's goal b as the session's turn ends, and says
// what to do next (goalTurn), with the binding as it is then.
func (h *memHost) goalTurnEnd(ctx context.Context, session string, b GoalBinding, marker string) (GoalTurn, GoalBinding, error) {
	res, err := h.call(ctx, memRequest{Code: "print(mem.JSON(" + strconv.Quote(b.Name) + "))", Session: session})
	if err != nil {
		return GoalTurn{}, b, err
	}
	if res.Instance != b.Instance {
		return GoalTurn{Do: "rebind", Notice: "mem was made anew, so the goal is bound again"}, b, nil
	}
	g, err := lastGoalJSON(res.Output)
	if err != nil {
		return GoalTurn{}, b, err
	}
	turn, b := goalTurn(b, g, h.now(), marker)
	if turn.Do == "judge" {
		res, err := h.call(ctx, memRequest{Code: "print(mem.Show(" + strconv.Quote(b.Name) + "))", Session: session})
		if err != nil {
			return GoalTurn{}, b, err
		}
		turn.Show = res.Output
	}
	return turn, b, nil
}

// goalTurn is MEMPLAN §7's table: what a turn's end does with goal g, bound
// as b, at now; marker is the line a reply ends with to stop.
func goalTurn(b GoalBinding, g memGoalJSON, now time.Time, marker string) (GoalTurn, GoalBinding) {
	today := now.Format(time.DateOnly)
	ref := "`" + b.Name + "` " + strconv.Quote(memLine(g.Title, 80))
	switch g.Status {
	case "done":
		return GoalTurn{Do: "judge"}, b
	case "dropped":
		return GoalTurn{Do: "clear", Notice: "The goal " + ref + " was dropped (" + memLine(g.Result, 120) + "), so /goal is cleared"}, b
	case "forgotten":
		return GoalTurn{Do: "clear", Notice: "The goal `" + b.Name + "` was forgotten, so /goal is cleared"}, b
	case "missing":
		return GoalTurn{Do: "clear", Notice: "The goal `" + b.Name + "` is no longer in mem (a change that added it was undone), so /goal is cleared"}, b
	}
	asked := b.Asked
	b.Asked = ""
	// A question for the user open under the goal ends the turn even when
	// a step is free: the steps after it may need the answer, which only
	// the user can give. (A real run went on for nine minutes looking for
	// a day the user had not yet chosen.)
	if asks := goalAsks(g); asks != "" {
		return GoalTurn{Do: "end", Notice: "The goal " + ref + " waits: " + asks + ". It stays set"}, b
	}
	switch {
	case g.Next == b.Name:
		return GoalTurn{Do: "continue", Prompt: "The goal " + ref + " is not closed, and none of its todos is open. Once it holds, close it with " +
			"`mem.Done(" + strconv.Quote(b.Name) + ", \"<command> → <result>\")`; otherwise add the todos still needed " +
			"(`mem.Todo(\"…\")`, standing in it) and do them."}, b
	case g.Next != "":
		return GoalTurn{Do: "continue", Prompt: "The goal " + ref + " is not done. Its next todo is `" + g.Next + "` " +
			strconv.Quote(memLine(g.NextTitle, 120)) + ": do it, and mark it with `mem.Done(" + strconv.Quote(g.Next) + ", \"…\")`. " +
			"Once the goal holds, close it with `mem.Done(" + strconv.Quote(b.Name) + ", \"<command> → <result>\")`."}, b
	}
	holds := goalHolds(g, now)
	// A wait on the goal holds back every item under it, which then says so.
	waits := g.Wait != "" && !(memIsDay(g.Wait) && g.Wait <= today)
	why := map[string]bool{}
	for _, h := range g.Held {
		why[h.Why] = true
	}
	switch {
	case why["words"] || waits:
		// Asked once; the same holds at the next turn's end end it.
		if asked == holds {
			return GoalTurn{Do: "end", Notice: "The goal " + ref + " waits: " + holds + ". It stays set"}, b
		}
		b.Asked = holds
		return GoalTurn{Do: "checkpoint", Prompt: "Nothing in the goal " + ref + " can be done now: " + holds + ". " +
			"If what it waits for has happened, clear the wait (`mem.Wait(name, \"\")`) and go on. If the goal already holds, " +
			"show it and close it with `mem.Done(" + strconv.Quote(b.Name) + ", \"…\")`. If there is a real reason to stop, say what " +
			"it is, then end your reply with this line on its own:\n" + marker}, b
	case why["session"]:
		return GoalTurn{Do: "end", Notice: "The goal " + ref + " waits on other sessions: " + holds + ". It stays set; `mem.Start(name)` in this session takes a step over"}, b
	}
	return GoalTurn{Do: "end", Notice: "The goal " + ref + " waits: " + holds + ". It stays set"}, b
}

// goalAsks names the questions for the user open under goal g, as
// goalHolds does: "`g/1/1` "which day?" is for the user"; "" for none.
func goalAsks(g memGoalJSON) string {
	var parts []string
	for _, a := range g.Asks {
		parts = append(parts, "`"+a.Name+"` "+strconv.Quote(memLine(a.Title, 80))+" is for the user")
	}
	return strings.Join(parts, "; ")
}

// memHeld is an item a goal's next passed over, and what holds it back:
// "user", "session", "day" or "words".
type memHeld struct {
	Name  string `json:"name"`
	Why   string `json:"why"`
	Title string `json:"title"`
	Wait  string `json:"wait"`
	Doing string `json:"doing"`
	// Changed is when it last changed: for one a session started, about
	// when that session started it or last worked on it.
	Changed int64 `json:"changed"`
}

// goalHolds says what holds back each item goal g's next passed over, as
// of now.
func goalHolds(g memGoalJSON, now time.Time) string {
	var parts []string
	for _, h := range g.Held {
		item := "`" + h.Name + "` " + strconv.Quote(memLine(h.Title, 80))
		switch h.Why {
		case "user":
			parts = append(parts, item+" is for the user")
		case "session":
			parts = append(parts, item+" was started by session "+memShortID(h.Doing)+", "+memAgo(now, h.Changed))
		case "day":
			parts = append(parts, item+" waits until "+h.Wait)
		default:
			parts = append(parts, item+" waits for "+strconv.Quote(memLine(h.Wait, 80)))
		}
	}
	return strings.Join(parts, "; ")
}

// memHeldRe is mem's refusal of a change to a frame a session holds, with
// the frame.
var memHeldRe = regexp.MustCompile("^`([^`]+)` is held by ")

// memShortID is a session's id as the user reads it: its first 8 bytes.
func memShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// memIsDay reports whether s is a day as mem writes one, 2026-12-01.
func memIsDay(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// goalReopen reopens goal b, which the judge found not met, as gnosh's own
// input, with what it found as a note.
func (h *memHost) goalReopen(ctx context.Context, b GoalBinding, reason string) error {
	code := "mem.Reopen(" + strconv.Quote(b.Name) + ")\nmem.Note(" + strconv.Quote(b.Name) + ", " +
		strconv.Quote("the check found it not met: "+memLine(reason, 300)) + ")"
	_, err := h.call(ctx, memRequest{Code: code})
	return err
}

// ClosedGoal is a goal a session closed as done, for the judge: every goal
// is judged before it counts as done (Jae, 2026-10-30), not only the one
// /goal binds.
type ClosedGoal struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	// Seq is the journal entry that closed it: closed again after it was
	// reopened, it is judged again.
	Seq int64 `json:"seq"`
	// Show is mem.Show(name): its steps, results and notes, the agent's
	// own claims.
	Show string `json:"show"`
}

// closedGoals are the goals session's own calls closed as done at since
// (Unix seconds) or later, and that are done still: the turn's, for the
// judge. except, the goal its /goal binds, is left out: that one is judged
// against the user's condition (goalTurnEnd).
func (h *memHost) closedGoals(ctx context.Context, session string, since int64, except string) ([]ClosedGoal, error) {
	closed := map[string]int64{}
	var names []string
	_, err := readEntriesOf(filepath.Join(h.dir, "journal.jsonl"), 0, 0, func(e memEntry, _ int64, _ string) error {
		if e.Who != "agent" || e.Session != session || e.Time < since {
			return nil
		}
		for _, op := range e.Ops {
			if op.Kind != "set" || op.Field != "status" || op.New != "done" || strings.Contains(op.Name, "/") || op.Name == except {
				continue
			}
			if _, ok := closed[op.Name]; !ok {
				names = append(names, op.Name)
			}
			closed[op.Name] = e.Seq
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) || len(names) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	res, err := h.call(ctx, memRequest{Code: "print(mem.JSON(" + strings.Join(quoted, ", ") + "))", Session: session})
	if err != nil {
		return nil, err
	}
	var j struct {
		Goals []memGoalJSON `json:"goals"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Output)), &j); err != nil {
		return nil, fmt.Errorf("mem's answer about the goals closed could not be read: %q", memLine(res.Output, 200))
	}
	var out []ClosedGoal
	for _, g := range j.Goals {
		if g.Status != "done" {
			continue
		}
		res, err := h.call(ctx, memRequest{Code: "print(mem.Show(" + strconv.Quote(g.Name) + "))", Session: session})
		if err != nil {
			return nil, err
		}
		out = append(out, ClosedGoal{Name: g.Name, Title: g.Title, Seq: closed[g.Name], Show: res.Output})
	}
	return out, nil
}

// goalClear unbinds b, as /goal clear does: the goal is dropped, "cleared
// by the user", if binding added it and it is still open. It says what it
// did.
func (h *memHost) goalClear(ctx context.Context, session string, b GoalBinding) (string, error) {
	if !b.Added {
		return "/goal is cleared; the goal `" + b.Name + "` stays in mem", nil
	}
	res, err := h.call(ctx, memRequest{Code: "print(mem.JSON(" + strconv.Quote(b.Name) + "))", Session: session})
	if err != nil {
		return "", err
	}
	g, err := lastGoalJSON(res.Output)
	if err != nil {
		return "", err
	}
	if res.Instance != b.Instance || g.Status != "" {
		return "/goal is cleared", nil
	}
	if _, err := h.call(ctx, memRequest{Code: "mem.Drop(" + strconv.Quote(b.Name) + ", \"cleared by the user\")", Session: session, User: true}); err != nil {
		if m := memHeldRe.FindStringSubmatch(err.Error()); m != nil {
			return "/goal is cleared; the goal `" + b.Name + "` it added stays open while the agent is in `" + m[1] + "`: it closes or drops it from there", nil
		}
		return "", err
	}
	return "/goal is cleared, and the goal `" + b.Name + "` it added is dropped", nil
}

// The Host's side of /goal: each runs under the host's lock, against the
// project's mem.

// BindGoal binds a session's /goal: see GoalBinding. It returns the
// binding, to keep with the session, and the directive to send.
func (h *Host) BindGoal(ctx context.Context, project, session, condition string, prev *GoalBinding) (GoalBinding, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	b, err := h.mem(project).bindGoal(ctx, session, condition, prev)
	if err != nil {
		return GoalBinding{}, "", err
	}
	return b, GoalDirective(b), nil
}

// GoalTurnEnd says what to do with a session's goal as its turn ends,
// with the binding as it is then. marker is the line a reply ends with to
// stop, which the checkpoint question names.
func (h *Host) GoalTurnEnd(ctx context.Context, project, session string, b GoalBinding, marker string) (GoalTurn, GoalBinding, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	return h.mem(project).goalTurnEnd(ctx, session, b, marker)
}

// GoalCheckpoint is what every tenth continuation of a goal asks instead
// of going on: whether there is a real reason to stop. There is no cap.
func GoalCheckpoint(b GoalBinding, continued int, marker string) string {
	return fmt.Sprintf("You have been sent on toward the goal `%[1]s` %[2]q %[3]d times. Step back: is there a real reason "+
		"to stop? For example, the goal cannot be reached as set; it is blocked on something only the user can give or "+
		"decide; or you are repeating the same attempts without progress. If the goal already holds, show it and close it "+
		"with `mem.Done(%[1]q, \"<command> → <result>\")`. If there is a reason to stop, explain it for the user, then end "+
		"your reply with this line on its own:\n%[4]s\nOtherwise keep working: many attempts are fine.",
		b.Name, memLine(b.Title, 80), continued, marker)
}

// GoalReopen reopens a goal the judge found not met.
func (h *Host) GoalReopen(ctx context.Context, project string, b GoalBinding, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	return h.mem(project).goalReopen(ctx, b, reason)
}

// ClosedGoals are the goals a session closed as done since its turn
// began, for the judge; see closedGoals.
func (h *Host) ClosedGoals(ctx context.Context, project, session string, since time.Time, except string) ([]ClosedGoal, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	return h.mem(project).closedGoals(ctx, session, since.Unix(), except)
}

// goal runs one of /goal's calls, as the worker gets them.
func (h *Host) goal(ctx context.Context, req goalReq) (goalResp, error) {
	var resp goalResp
	var err error
	switch req.Op {
	case "bind":
		resp.Binding, resp.Text, err = h.BindGoal(ctx, req.Project, req.Session, req.Condition, req.Prev)
	case "turn":
		resp.Turn, resp.Binding, err = h.GoalTurnEnd(ctx, req.Project, req.Session, req.Binding, req.Marker)
	case "reopen":
		err = h.GoalReopen(ctx, req.Project, req.Binding, req.Reason)
	case "closed":
		resp.Closed, err = h.ClosedGoals(ctx, req.Project, req.Session, time.Unix(req.Since, 0), req.Except)
	case "clear":
		resp.Text, err = h.GoalClear(ctx, req.Project, req.Session, req.Binding)
	default:
		err = fmt.Errorf("unknown goal call %q", req.Op)
	}
	return resp, err
}

// GoalClear unbinds a session's /goal; see goalClear.
func (h *Host) GoalClear(ctx context.Context, project, session string, b GoalBinding) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	return h.mem(project).goalClear(ctx, session, b)
}
