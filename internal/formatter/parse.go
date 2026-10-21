// Package formatter turns a Wispr markdown summary into a Telegram HTML message.
//
// Parsing is deliberately simple and table-driven: it reads Wispr's own
// sections ("Принятые решения", "Следующие шаги", ...) and never invents
// content. An LLM-backed extractor can later replace Parse behind the same
// Parsed result.
package formatter

import (
	"regexp"
	"strings"
	"unicode"
)

// Task is one action item; Assignee is empty when Wispr did not name one.
type Task struct {
	Assignee string
	Text     string
}

// Topic is a discussion section: its heading and first bullet.
type Topic struct {
	Heading string
	Gist    string
}

// Parsed is the structured content extracted from a summary.
type Parsed struct {
	Overview  string // Wispr's own one-paragraph recap that precedes the first heading
	Topics    []Topic
	Decisions []string
	Tasks     []Task
}

// Heading phrases (lowercase). A section is a task/decision list only when its
// heading STARTS with one of them, so topics like "Первые шаги онбординга" or
// "Решение проблемы с JWT" are not misread. Extend here for new languages.
var (
	taskHeadings     = []string{"следующие шаги", "дальнейшие шаги", "next step", "action item", "to-do", "todo"}
	decisionHeadings = []string{"принятые решения", "решения", "ключевые решения", "decisions", "key decisions"}
)

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	itemRe     = regexp.MustCompile(`^(\s*)(?:[-*+•]|\d+[.)])\s+(.*)$`)
	checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s*`)
	assigneeRe = regexp.MustCompile(`^\(([^()]{1,60})\)\s*(.+)$`)
	numberRe   = regexp.MustCompile(`^\d+(\.\d+)*[.)]?\s+`)
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	boldRe     = regexp.MustCompile(`\*\*`)
	// Emphasis only when the asterisks hug text and sit at word boundaries, so
	// globs like "*.log и *.tmp" are left alone.
	italicRe = regexp.MustCompile(`(^|[\s(])\*([^*\s](?:[^*\n]*[^*\s])?)\*($|[\s).,;:!?])`)
	spacesRe = regexp.MustCompile(`\s+`)
)

type item struct {
	text string
	subs []string // indented sub-bullets
}

type section struct {
	heading string
	items   []item
}

// Parse extracts topics, decisions and tasks from a markdown summary.
// Empty or unstructured input yields an empty Parsed.
func Parse(summary string) Parsed {
	var p Parsed
	secs, intro := splitSections(summary)
	p.Overview = intro
	for _, s := range secs {
		if len(s.items) == 0 {
			continue
		}
		switch kindOf(s.heading) {
		case kindTasks:
			for _, it := range s.items {
				p.Tasks = append(p.Tasks, parseTask(it.full()))
			}
		case kindDecisions:
			for _, it := range s.items {
				p.Decisions = append(p.Decisions, it.full())
			}
		default:
			p.Topics = append(p.Topics, Topic{Heading: s.heading, Gist: s.items[0].text})
		}
	}
	return p
}

// full joins an item with its sub-bullets so details stay with their parent.
func (it item) full() string {
	if len(it.subs) == 0 {
		return it.text
	}
	return it.text + " — " + strings.Join(it.subs, "; ")
}

// splitSections returns the sections plus the plain paragraph(s) before the first heading.
func splitSections(md string) ([]section, string) {
	var secs []section
	var intro []string
	cur := section{}
	flush := func() {
		if cur.heading != "" || len(cur.items) > 0 {
			secs = append(secs, cur)
		}
	}
	for _, line := range strings.Split(md, "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = section{heading: clean(numberRe.ReplaceAllString(clean(m[1]), ""))}
			continue
		}
		m := itemRe.FindStringSubmatch(line)
		if m == nil {
			if len(secs) == 0 && cur.heading == "" && len(cur.items) == 0 {
				if t := clean(line); t != "" {
					intro = append(intro, t)
				}
			}
			continue
		}
		text := clean(checkboxRe.ReplaceAllString(strings.TrimSpace(m[2]), ""))
		if text == "" {
			continue
		}
		indent := len(strings.ReplaceAll(m[1], "\t", "    "))
		if indent >= 2 && len(cur.items) > 0 {
			last := &cur.items[len(cur.items)-1]
			last.subs = append(last.subs, text)
			continue
		}
		cur.items = append(cur.items, item{text: text})
	}
	flush()
	return secs, strings.Join(intro, " ")
}

type kind int

const (
	kindTopic kind = iota
	kindTasks
	kindDecisions
)

func kindOf(heading string) kind {
	h := strings.ToLower(strings.TrimSpace(heading))
	if hasAnyPrefix(h, taskHeadings) {
		return kindTasks
	}
	if hasAnyPrefix(h, decisionHeadings) {
		return kindDecisions
	}
	return kindTopic
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func parseTask(item string) Task {
	if m := assigneeRe.FindStringSubmatch(item); m != nil && looksLikeName(m[1]) {
		return Task{Assignee: strings.TrimSpace(m[1]), Text: strings.TrimSpace(m[2])}
	}
	return Task{Text: item}
}

var nameSepRe = regexp.MustCompile(`\s*(?:,|&|\s+и\s+|\s+and\s+)\s*`)

// looksLikeName accepts "Иван", "Иван, Дмитрий", "Иван и Дмитрий", "Все
// участники" and rejects remarks such as "(если успеем)" or "(срочно)": each
// listed name must start with an uppercase letter, at most 8 words in total.
func looksLikeName(s string) bool {
	if len(strings.Fields(s)) > 8 {
		return false
	}
	for _, part := range nameSepRe.Split(strings.TrimSpace(s), -1) {
		r := []rune(part)
		if len(r) == 0 || !unicode.IsUpper(r[0]) {
			return false
		}
	}
	return true
}

// clean strips inline markdown (links, bold, emphasis, code) and collapses whitespace.
func clean(s string) string {
	s = linkRe.ReplaceAllString(s, "$1")
	s = boldRe.ReplaceAllString(s, "")
	for range 2 { // second pass catches emphasis sharing a boundary: "*a* *b*"
		s = italicRe.ReplaceAllString(s, "${1}${2}${3}")
	}
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimSpace(spacesRe.ReplaceAllString(s, " "))
}
