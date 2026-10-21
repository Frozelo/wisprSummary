package formatter

import (
	"fmt"
	"html"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxMessageLen is Telegram's per-message text limit.
const MaxMessageLen = 4096

const (
	maxTopics      = 4
	maxLine        = 400 // a single bullet is never longer, so one always fits a message
	maxDecisionsSm = 5
)

// Meeting is the input for one notification.
type Meeting struct {
	Title     string
	Start     time.Time
	ShareLink string
	Summary   string // Wispr markdown summary
}

// Format builds the Telegram HTML message(s) for a meeting. It returns more
// than one message only when the content cannot fit in MaxMessageLen. loc is
// used for the date (nil = local time).
func Format(m Meeting, loc *time.Location) []string {
	if loc == nil {
		loc = time.Local
	}
	p := Parse(m.Summary)

	head := []string{"<b>Встреча:</b> " + esc(orDefault(m.Title, "Без названия"))}
	if !m.Start.IsZero() {
		head = append(head, "<b>Дата:</b> "+m.Start.In(loc).Format("02.01.2006"))
	}
	var foot []string
	if strings.HasPrefix(m.ShareLink, "http://") || strings.HasPrefix(m.ShareLink, "https://") {
		foot = []string{"", "<b>Материалы:</b>", `<a href="` + esc(m.ShareLink) + `">Заметка в Wispr Flow</a>`}
	}

	topics := topicLines(p.Topics)
	decisions := decisionLines(p.Decisions, 0)
	tasks := taskLines(p.Tasks)

	if len(topics) == 0 && len(decisions) == 0 && len(tasks) == 0 {
		body := []string{"", "<i>Summary пока недоступно.</i>"}
		return pack(head, body, foot)
	}

	// Progressively shrink: full -> no overview -> short decisions -> split.
	for _, body := range [][]string{
		join(topics, decisions, tasks),
		join(nil, decisions, tasks),
		join(nil, decisionLines(p.Decisions, maxDecisionsSm), tasks),
	} {
		if msgs := pack(head, body, foot); len(msgs) == 1 {
			return msgs
		}
	}
	return pack(head, join(nil, decisions, tasks), foot)
}

func topicLines(ts []Topic) []string {
	if len(ts) == 0 {
		return nil
	}
	out := []string{"", "<b>Краткие итоги:</b>"}
	for i, t := range ts {
		if i == maxTopics {
			break
		}
		line := "• " + esc(trunc(t.Gist, maxLine))
		if t.Heading != "" {
			line = "• <b>" + esc(trunc(t.Heading, 120)) + "</b>: " + esc(trunc(t.Gist, maxLine))
		}
		out = append(out, line)
	}
	return out
}

// decisionLines lists decisions; limit > 0 caps the count and notes the rest.
func decisionLines(ds []string, limit int) []string {
	if len(ds) == 0 {
		return nil
	}
	out := []string{"", "<b>Принятые решения:</b>"}
	for i, d := range ds {
		if limit > 0 && i == limit {
			out = append(out, fmt.Sprintf("<i>…и ещё %d</i>", len(ds)-limit))
			break
		}
		out = append(out, "• "+esc(trunc(d, maxLine)))
	}
	return out
}

// taskLines groups tasks by assignee (first-appearance order); unassigned
// tasks go last. With no assignees at all the list is flat.
func taskLines(ts []Task) []string {
	if len(ts) == 0 {
		return nil
	}
	out := []string{"", "<b>Задачи:</b>"}
	var order []string
	groups := map[string][]string{}
	for _, t := range ts {
		if _, ok := groups[t.Assignee]; !ok {
			order = append(order, t.Assignee)
		}
		groups[t.Assignee] = append(groups[t.Assignee], "• "+esc(trunc(t.Text, maxLine)))
	}
	if len(order) == 1 && order[0] == "" {
		return append(out, groups[""]...)
	}
	for _, a := range order {
		if a != "" {
			out = append(out, "<b>"+esc(trunc(a, 60))+":</b>")
			out = append(out, groups[a]...)
		}
	}
	if g, ok := groups[""]; ok {
		out = append(out, "<b>Без ответственного:</b>")
		out = append(out, g...)
	}
	return out
}

// pack joins head+body+foot into one message, or splits the body by lines
// into several when it exceeds MaxMessageLen. The footer goes last.
func pack(head, body, foot []string) []string {
	if one := strings.Join(join(head, body, foot), "\n"); utf8.RuneCountInString(one) <= MaxMessageLen {
		return []string{one}
	}
	var msgs []string
	cur := append([]string(nil), head...)
	size := runes(cur)
	for _, line := range join(body, foot) {
		if size+utf8.RuneCountInString(line)+1 > MaxMessageLen {
			msgs = append(msgs, strings.Join(cur, "\n"))
			cur = []string{"<i>(продолжение)</i>"}
			size = runes(cur)
		}
		cur = append(cur, line)
		size += utf8.RuneCountInString(line) + 1
	}
	return append(msgs, strings.Join(cur, "\n"))
}

func runes(lines []string) int {
	n := 0
	for _, l := range lines {
		n += utf8.RuneCountInString(l) + 1
	}
	return n
}

func join(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func esc(s string) string { return html.EscapeString(s) }

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// trunc shortens s to n runes. It runs before escaping so tags/entities are never cut.
func trunc(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
