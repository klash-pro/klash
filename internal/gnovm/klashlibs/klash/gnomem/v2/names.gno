// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Names. One grammar covers items, facts and rules: segments joined by "/";
// a segment is words of [a-z0-9] joined by single "-"; the first segment
// has a letter. A name is typed exactly as printed; anything else is
// refused with the name it should have been.
//
// An item's id is its goal's name, then its steps' names:
// release-v2/bump/version. A goal's, a step's or a thought's name is one
// segment of at most 32 bytes (a goal's of at most 4 words), made from its
// title or given; a goal's is never reserved and never given twice. A
// fact's or a rule's name is any name of at most 64 bytes.
const (
	maxGoalName = 32
	maxName     = 64
)

// reserved are names no goal may take.
var reserved = map[string]bool{"all": true, "next": true, "none": true}

// filler are the words a made name leaves out.
var filler = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true,
	"by": true, "for": true, "from": true, "in": true, "is": true, "it": true, "its": true,
	"my": true, "of": true, "on": true, "or": true, "our": true, "that": true, "the": true,
	"this": true, "to": true, "with": true, "your": true,
}

// validName reports whether s follows the name grammar.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, seg := range strings.Split(s, "/") {
		if seg == "" || seg[0] == '-' || seg[len(seg)-1] == '-' || strings.Contains(seg, "--") {
			return false
		}
		letter := false
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z':
				letter = true
			case r >= '0' && r <= '9', r == '-':
			default:
				return false
			}
		}
		if i == 0 && !letter {
			return false
		}
	}
	return true
}

// nameForm is s as a name: lowercase words of [a-z0-9] joined by "-", with
// ' and ’ deleted and Latin accents folded. Redact matches names with it.
func nameForm(s string) string {
	return strings.Join(words(s), "-")
}

func words(s string) []string {
	var out []string
	var w strings.Builder
	flush := func() {
		if w.Len() > 0 {
			out = append(out, w.String())
			w.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		r = fold(r)
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			w.WriteRune(r)
		case r == '\'' || r == '’':
		default:
			flush()
		}
	}
	flush()
	return out
}

// fold turns a Latin letter with an accent into its plain letter.
func fold(r rune) rune {
	const from, to = "àáâãäåçèéêëìíîïñòóôõöùúûüýÿ", "aaaaaaceeeeiiiinooooouuuuyy"
	if i := strings.IndexRune(from, r); i >= 0 {
		return []rune(to)[utf8.RuneCountInString(from[:i])]
	}
	return r
}

// nameOf makes a name from text: filler words dropped, words that look
// like keys or hashes skipped (so a token in a title rarely reaches a
// name), at most 4 words and 32 bytes; fallback names it when the text has
// no words to use ("todo", "note").
func nameOf(title, fallback string) string {
	all := nameWords(title)
	var kept []string
	for _, w := range all {
		if !filler[w] && !keyLike(w) {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		for _, w := range all {
			if !keyLike(w) {
				kept = append(kept, w)
			}
		}
	}
	name := ""
	for i, w := range kept {
		next := w
		if name != "" {
			next = name + "-" + w
		}
		if i == 4 || len(next) > maxGoalName {
			break
		}
		name = next
	}
	switch {
	case name == "":
		return fallback
	case !validName(name):
		return fallback + "-" + name
	}
	return name
}

// nameWords are a title's words for a name: words(), except that a
// hyphenated word of letters ("on-call", "re-plan") stays one word, so its
// filler half is not dropped and "the on-call engineer" names on-call-engineer.
func nameWords(title string) []string {
	var out []string
	for _, tok := range strings.Fields(title) {
		if c := compound(tok); c != "" {
			out = append(out, c)
			continue
		}
		out = append(out, words(tok)...)
	}
	return out
}

// compound is tok lowercased when it is letters joined by hyphens, any
// punctuation around it dropped, and "" otherwise.
func compound(tok string) string {
	tok = strings.ToLower(strings.Trim(tok, "()[]{}<>\"'`.,;:!?"))
	parts := strings.Split(tok, "-")
	if len(parts) < 2 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			if r < 'a' || r > 'z' {
				return ""
			}
		}
	}
	return strings.Join(parts, "-")
}

// keyLike reports whether w looks like a key or a hash: over 20 bytes, or
// over 8 bytes mixing letters and digits.
func keyLike(w string) bool {
	if len(w) > 20 {
		return true
	}
	if len(w) <= 8 {
		return false
	}
	letters, digits := false, false
	for _, r := range w {
		if r >= '0' && r <= '9' {
			digits = true
		} else {
			letters = true
		}
	}
	return letters && digits
}

// freeTop reports whether n can name a new top-level todo: it is not
// reserved, not a forgotten one's, and was never given before.
func (m *Mem) freeTop(n string) bool {
	return !reserved[n] && !forgottenName(n) && m.taken.Get(n) == nil
}

func (m *Mem) freeName(base string, free func(string) bool) string {
	if free(base) {
		return base
	}
	for i := 2; ; i++ {
		if n := base + "-" + strconv.Itoa(i); free(n) {
			return n
		}
	}
}

func forgottenName(n string) bool {
	return strings.HasPrefix(n, "forgotten-")
}

// refuseOpenTitle refuses a new top-level item whose title an open one has.
func (m *Mem) refuseOpenTitle(title string) {
	if n := m.titled(title); n != "" {
		it := m.get(n)
		aside := who(it.By) + ", " + ago(it.Changed)
		if len(it.Steps) > 0 {
			aside += ", " + progress(it)
		}
		panic(ref(n, it.Title) + " (" + aside + ") is already open: step into it with mem.Frame(\"" + n + "\").Push() and add to it with mem.Todo(\"…\"), or give other work a task that tells them apart")
	}
}

// checkName refuses a fact's or a rule's name outside the grammar.
func (m *Mem) checkName(name, kind string) {
	switch {
	case name == "":
		panic("a " + kind + " needs a name, like `cmd/test`")
	case strings.ToLower(name) != name && validName(strings.ToLower(name)):
		panic("names are lowercase: `" + strings.ToLower(name) + "`")
	case !validName(name):
		example := "cmd/test"
		if kind == "rule" {
			example = "push-after-landing"
		}
		panic(named(name) + " is not a name: a " + kind + "'s name is lowercase words joined by \"-\" and \"/\"" + like(pathForm(name), example))
	case len(name) > maxName:
		panic("a " + kind + "'s name is at most " + strconv.Itoa(maxName) + " bytes")
	}
}

// item is item name, refusing a name that is not one, with what it should
// have been, and a forgotten item.
func (m *Mem) item(name string) *Todo {
	if it := m.get(name); it != nil && validName(name) {
		if it.Status == "forgotten" {
			panic("`" + name + "` is forgotten")
		}
		return it
	}
	panic(m.miss(name))
}

// openItem is open item name.
func (m *Mem) openItem(name string) *Todo {
	it := m.item(name)
	if it.Status != "" {
		panic(ref(name, it.Title) + " is " + it.Status + arrow(it.Result) + "; mem.Reopen(\"" + name + "\") to work on it again")
	}
	return it
}

// miss says why name names no item, and what it might have been.
func (m *Mem) miss(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	switch {
	case name == "":
		return "no item named \"\": mem.Show() lists them"
	case low != name && m.get(low) != nil:
		return "names are lowercase, typed as printed: `" + low + "`"
	case strings.Contains(name, "#"):
		item, thought := splitAddr(name)
		return "`" + name + "` is a thought's address, not an item's: mem.Thought(" + strconv.Quote(thought) + ") reads it, and mem.Show(" + strconv.Quote(item) + ") shows its frame"
	case isNumber(strings.TrimLeft(name, "#")):
		return "`" + name + "` is not a name; todos are named under the one they are in, like `fix-flaky/find-cause`: mem.Show() lists them"
	case m.getFact(name) != nil:
		return "`" + name + "` is a fact, not an item; to change it: mem.Fact(\"" + name + "\", \"…\")"
	case m.getRule(name) != nil:
		return "`" + name + "` is a rule, not an item; to change it: mem.Rule(\"" + name + "\", \"…\")"
	case m.formed(name) != "":
		return "no item `" + name + "` (a name joins its words with \"-\": did you mean `" + m.formed(name) + "`?)"
	case m.titled(name) != "":
		return strconv.Quote(name) + " is a task, not a name: the todo with that task is `" + m.titled(name) + "`"
	case m.get(goalOf(name)) == nil && m.slugged(goalOf(name)) != "":
		g := goalOf(name)
		msg := "`" + g + "` is not a name: a todo's name keeps at most 4 words of its task, and this one's is `" + m.slugged(g) + "`"
		if rest := name[len(g):]; rest != "" {
			msg += ", so the todo is `" + m.slugged(g) + rest + "`"
		}
		return msg
	}
	if p := parentOf(name); p != "" {
		if it := m.get(p); it != nil {
			if len(it.Steps) == 0 {
				return "no item `" + name + "`; `" + p + "` has no todos"
			}
			var ids []string
			for _, s := range it.Steps {
				ids = append(ids, p+"/"+s)
			}
			if len(ids) == 1 {
				return "no item `" + name + "`; `" + p + "` has the todo `" + ids[0] + "`"
			}
			return "no item `" + name + "`; `" + p + "` has the todos " + names(ids)
		}
	}
	return "no item `" + name + "`; mem.Show() lists them"
}

// formed is name with each of its parts in a name's form, as
// `release-1-3-0/1` for `release-1.3.0/1`, when that names an item; or "".
func (m *Mem) formed(name string) string {
	if f := pathForm(name); f != name && m.get(f) != nil {
		return f
	}
	return ""
}

// pathForm is s with each part between "/" in a name's form, and the empty
// parts dropped: `cmd/test` for "cmd//test", `cafe/menu` for "café/menu".
func pathForm(s string) string {
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if f := nameForm(p); f != "" {
			parts = append(parts, f)
		}
	}
	return strings.Join(parts, "/")
}

// named is a name as a refusal shows it: in backticks, or Go-quoted when it
// holds what would not show, a tab or a byte that is not UTF-8.
func named(name string) string {
	if q := strconv.Quote(name); q[1:len(q)-1] != name {
		return q
	}
	return "`" + name + "`"
}

// like ends a refusal of a name with the name it might have been, or, when
// it has none, an example; and says so when its first part lacks a letter,
// as a name's must have.
func like(suggestion, example string) string {
	switch {
	case validName(suggestion):
		return ", like `" + suggestion + "`"
	case suggestion != "":
		return ", the first with a letter in it, like `" + example + "`"
	}
	return ", like `" + example + "`"
}

// titled is the name of the open goal titled title, or "": one lookup in
// the titles index.
func (m *Mem) titled(title string) string {
	// Keys run title, "|", name: a title with a "|" of its own, "Support
	// json|yaml", falls in the range of "Support json", so each is
	// compared whole.
	found, want := "", sameTitle(title)
	m.titles.Iterate(want+"|", want+"|\xff", func(_ string, v any) bool {
		if n := v.(string); sameTitle(m.get(n).Title) == want {
			found = n
		}
		return found != ""
	})
	return found
}

// slugged is the name of the open goal whose title's every word, joined by
// "-", is name, as a model makes a name; or "".
func (m *Mem) slugged(name string) string {
	found := ""
	m.open.Iterate("", "", func(_ string, v any) bool {
		if n := v.(string); nameForm(m.get(n).Title) == name {
			found = n
		}
		return found != ""
	})
	return found
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// text refuses text that is not valid UTF-8, and trims it.
func text(what, s string) string {
	if !utf8.ValidString(s) {
		panic(what + " is not valid UTF-8")
	}
	return strings.TrimSpace(s)
}

// clip cuts s to at most n bytes, at a character, with "…".
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// overlapsMarker reports whether redacting s could leave s behind: s holds
// "(redacted)", lies inside it, or begins or ends partway into it.
func overlapsMarker(s string) bool {
	if strings.Contains(s, redacted) || strings.Contains(redacted, s) {
		return true
	}
	for i := 1; i < len(redacted); i++ {
		if strings.HasSuffix(s, redacted[:i]) || strings.HasPrefix(s, redacted[i:]) {
			return true
		}
	}
	return false
}
