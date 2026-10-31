package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// titleModel answers title requests with fixed text and a fixed finish
// reason, and records the options each request carried.
type titleModel struct {
	fakeLanguageModel
	text   string
	finish fantasy.FinishReason

	mu    sync.Mutex
	calls []fantasy.Call
}

func (m *titleModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		if m.text != "" {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: m.text})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t"})
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: m.finish})
	}, nil
}

func (m *titleModel) recorded() []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Call(nil), m.calls...)
}

// titleAgent builds a session agent whose small and large models are both
// llm, served by an OpenAI-compatible provider "qwen", as on the AI box.
func titleAgent(t *testing.T, llm fantasy.LanguageModel) (*sessionAgent, fakeEnv, string) {
	t.Helper()
	env := testEnv(t)
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("qwen", config.ProviderConfig{ID: "qwen", Type: "llamacpp"})
	cfg := &config.Config{Providers: providers, Options: &config.Options{}}
	model := Model{
		Model:      llm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Provider: "qwen", Model: "qwen3.8-27b"},
	}
	a := NewSessionAgent(SessionAgentOptions{
		LargeModel: model,
		SmallModel: model,
		Sessions:   env.sessions,
		Messages:   env.messages,
		Cfg:        config.NewTestStore(cfg),
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)
	return a, env, sess.ID
}

// The cause of every session on the AI box being "Untitled Session": the
// title request went out with no thinking switch, so Qwen reasoned through
// its whole budget. Each attempt must carry enable_thinking false.
func TestGenerateTitleSwitchesThinkingOff(t *testing.T) {
	t.Parallel()

	llm := &titleModel{text: "Fix the flaky test", finish: fantasy.FinishReasonStop}
	a, env, id := titleAgent(t, llm)
	a.GenerateTitle(t.Context(), id, "please fix the flaky test in the agent package")

	calls := llm.recorded()
	require.NotEmpty(t, calls)
	for _, call := range calls {
		kw, ok := thinkingKwargs(t, call.ProviderOptions)
		require.True(t, ok, "the title request must carry the chat-template switch")
		require.Equal(t, false, kw["enable_thinking"])
	}

	sess, err := env.sessions.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "Fix the flaky test", sess.Title)
}

// A session named before its first prompt, with gnosh -n, keeps the name,
// as Claude Code keeps a --name: no title is asked for. One created with a
// stand-in title is titled.
func TestGenerateTitleKeepsAGivenName(t *testing.T) {
	t.Parallel()

	llm := &titleModel{text: "Fix the flaky test", finish: fantasy.FinishReasonStop}
	a, env, id := titleAgent(t, llm)
	require.NoError(t, env.sessions.Rename(t.Context(), id, "nightly check"))
	a.GenerateTitle(t.Context(), id, "please fix the flaky test in the agent package")
	sess, err := env.sessions.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "nightly check", sess.Title)
	require.Empty(t, llm.recorded(), "no title is asked for")

	for _, untitled := range []string{session.NewTitle, session.RunTitle, session.UntitledTitle} {
		require.NoError(t, env.sessions.Rename(t.Context(), id, untitled))
		a.GenerateTitle(t.Context(), id, "please fix the flaky test in the agent package")
		sess, err = env.sessions.Get(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, "Fix the flaky test", sess.Title, "created as %q", untitled)
	}
}

// If every model is still cut off, the session is named after the start of
// the prompt, not a generic name that makes it impossible to find later.
func TestGenerateTitleFallsBackToThePrompt(t *testing.T) {
	t.Parallel()

	llm := &titleModel{finish: fantasy.FinishReasonLength}
	a, env, id := titleAgent(t, llm)
	a.GenerateTitle(t.Context(), id, "make a short video of the fromhebrew site\nwith music")

	sess, err := env.sessions.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "make a short video of the fromhebrew site with mu…", sess.Title,
		"truncated to 50 columns, the ellipsis included")
	require.NotEqual(t, DefaultSessionName, sess.Title)
	require.Len(t, llm.recorded(), 2, "both the small and the large model are tried first")
}

// A sub-agent's session is never listed or shown, so it gets no title
// request. On the AI box that request ran beside the sub-agent's first one,
// took the model server's last free slot and evicted the parent's prompt
// cache; each time the parent resumed it re-read its 110K-token
// conversation, about seven minutes.
func TestRunTitlesOnlyTopLevelSessions(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	small := &titleModel{text: "Fix the flaky test", finish: fantasy.FinishReasonStop}
	a := testSessionAgent(env, fastModel{}, small, "system")

	parent, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "call-1", parent.ID, "New Agent Session")
	require.NoError(t, err)

	_, err = a.Run(t.Context(), SessionAgentCall{SessionID: child.ID, Prompt: "find where the config is loaded", NonInteractive: true})
	require.NoError(t, err)

	// The parent's first prompt is still titled. By the time its title is
	// in, a title request for the child would have been made too.
	_, err = a.Run(t.Context(), SessionAgentCall{SessionID: parent.ID, Prompt: "please fix the flaky test"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		sess, err := env.sessions.Get(t.Context(), parent.ID)
		return err == nil && sess.Title == "Fix the flaky test"
	}, 5*time.Second, 10*time.Millisecond)

	require.Equal(t, 1, len(small.recorded()), "only the top-level session asks for a title")
	sess, err := env.sessions.Get(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, "New Agent Session", sess.Title)
}

func TestTitleFromPrompt(t *testing.T) {
	t.Parallel()

	require.Equal(t, "short prompt", titleFromPrompt("  short prompt  "))
	require.Equal(t, "two lines", titleFromPrompt("two\nlines"))
	require.Equal(t, DefaultSessionName, titleFromPrompt(""))
	require.Equal(t, DefaultSessionName, titleFromPrompt(" \n "))
	long := titleFromPrompt("abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij abcdefghij")
	require.LessOrEqual(t, len([]rune(long)), 50)
	require.Contains(t, long, "…")
}

// A provider without the switch gets no options added, and an agent
// without a config does not panic.
func TestTitleProviderOptionsWithoutSwitch(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("anthropic", config.ProviderConfig{ID: "anthropic", Type: "anthropic"})
	a := NewSessionAgent(SessionAgentOptions{
		Cfg: config.NewTestStore(&config.Config{Providers: providers, Options: &config.Options{}}),
	}).(*sessionAgent)
	require.Empty(t, a.titleProviderOptions(Model{ModelCfg: config.SelectedModel{Provider: "anthropic"}}))

	bare := NewSessionAgent(SessionAgentOptions{}).(*sessionAgent)
	require.Empty(t, bare.titleProviderOptions(Model{ModelCfg: config.SelectedModel{Provider: "qwen"}}))
}

// titleWatchModel is a main model that records, as its turn starts, the
// session's title and whether the small model was asked for one yet.
type titleWatchModel struct {
	fastModel
	small    *titleModel
	sessions func() string
	mu       sync.Mutex
	titles   []string
	asked    []int
}

func (m *titleWatchModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.titles = append(m.titles, m.sessions())
	m.asked = append(m.asked, len(m.small.recorded()))
	m.mu.Unlock()
	return m.fastModel.Stream(ctx, call)
}

// A new session is named after its prompt at once, and its title is asked
// for only when the turn ends: asked for at the start, it took the model
// server slot whose cache the turn needed.
func TestRunTitlesAfterTheTurn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	small := &titleModel{text: "Fix the flaky test", finish: fantasy.FinishReasonStop}
	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)
	large := &titleWatchModel{small: small, sessions: func() string {
		s, _ := env.sessions.Get(context.Background(), sess.ID)
		return s.Title
	}}
	a := testSessionAgent(env, large, small, "system")

	_, err = a.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "please fix the flaky test"})
	require.NoError(t, err)
	large.mu.Lock()
	require.Equal(t, "please fix the flaky test", large.titles[0], "named after the prompt at once")
	require.Equal(t, 0, large.asked[0], "no title asked for before the turn")
	large.mu.Unlock()
	require.Eventually(t, func() bool {
		s, err := env.sessions.Get(t.Context(), sess.ID)
		return err == nil && s.Title == "Fix the flaky test"
	}, 5*time.Second, 10*time.Millisecond, "titled by the model after it")
}

// memName makes a mem name of what the small model answers, however it
// dresses it, and nothing of an answer that is not one.
func TestMemName(t *testing.T) {
	t.Parallel()
	for answer, want := range map[string]string{
		"shared-port":                         "shared-port",
		"Shared Port\n\nbecause it is shared": "shared-port",
		"`staging-db`.":                       "staging-db",
		"the port is read from an env var":    "the-port-is-read",
		"\"version lives in version.go\"":     "version-lives-in-version",
		"2fa setup":                           "",
		"":                                    "",
		"---":                                 "",
	} {
		require.Equal(t, want, memName(answer), "%q", answer)
	}
}

func TestMemNames(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"code", "tests", "readme-and-docs"}, memNames("code\ntests\nREADME and docs\n", 3))
	require.Equal(t, []string{"code", "tests"}, memNames("1. code\n2. tests", 2), "numbered lines name all the same")
	require.Equal(t, []string{"code", ""}, memNames("code\n---", 2), "a line that is no name gives none")
	require.Nil(t, memNames("code\ntests", 3), "too few lines is no answer")
	require.Nil(t, memNames("code tests readme", 3), "one line for three is no answer")
}
