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
	Topics    []Topic
	Decisions []string
	Tasks     []Task
}

// Heading keywords (lowercase substrings). Extend here for new languages.
var (
	taskHeadings     = []string{"шаги", "next step", "action item", "to-do", "todo"}
	decisionHeadings = []string{"решени", "decision"}
)

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	itemRe     = regexp.MustCompile(`^\s*(?:[-*+•]|\d+[.)])\s+(.*)$`)
	checkboxRe = regexp.MustCompile(`^\[[ xX]\]\s*`)
	assigneeRe = regexp.MustCompile(`^\(([^()]{1,60})\)\s*(.+)$`)
	numberRe   = regexp.MustCompile(`^\d+(\.\d+)*[.)]?\s+`)
	linkRe     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	boldRe     = regexp.MustCompile(`\*\*|__`)
	italicRe   = regexp.MustCompile(`\*([^*\n]+)\*`)
	spacesRe   = regexp.MustCompile(`\s+`)
)

type section struct {
	heading string
	items   []string
}

// Parse extracts topics, decisions and tasks from a markdown summary.
// Empty or unstructured input yields an empty Parsed.
func Parse(summary string) Parsed {
	var p Parsed
	for _, s := range splitSections(summary) {
		if len(s.items) == 0 {
			continue
		}
		switch kindOf(s.heading) {
		case kindTasks:
			for _, it := range s.items {
				p.Tasks = append(p.Tasks, parseTask(it))
			}
		case kindDecisions:
			p.Decisions = append(p.Decisions, s.items...)
		default:
			p.Topics = append(p.Topics, Topic{Heading: s.heading, Gist: s.items[0]})
		}
	}
	return p
}

func splitSections(md string) []section {
	var secs []section
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
		if m := itemRe.FindStringSubmatch(line); m != nil {
			if t := clean(checkboxRe.ReplaceAllString(strings.TrimSpace(m[1]), "")); t != "" {
				cur.items = append(cur.items, t)
			}
		}
	}
	flush()
	return secs
}

type kind int

const (
	kindTopic kind = iota
	kindTasks
	kindDecisions
)

func kindOf(heading string) kind {
	h := strings.ToLower(heading)
	if containsAny(h, taskHeadings) {
		return kindTasks
	}
	if containsAny(h, decisionHeadings) {
		return kindDecisions
	}
	return kindTopic
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func parseTask(item string) Task {
	if m := assigneeRe.FindStringSubmatch(item); m != nil {
		return Task{Assignee: strings.TrimSpace(m[1]), Text: strings.TrimSpace(m[2])}
	}
	return Task{Text: item}
}

// clean strips inline markdown (links, bold, italics, code) and collapses whitespace.
func clean(s string) string {
	s = linkRe.ReplaceAllString(s, "$1")
	s = boldRe.ReplaceAllString(s, "")
	s = italicRe.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimSpace(spacesRe.ReplaceAllString(s, " "))
}
