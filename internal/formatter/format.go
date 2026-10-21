package formatter

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
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
	maxTitle       = 200
	maxOverview    = 600
)

// Meeting is the input for one notification.
type Meeting struct {
	Title     string
	Start     time.Time
	ShareLink string
	Summary   string // Wispr markdown summary
}

// Message is one Telegram message of a notification. Tasks lists the 1-based
// numbers of the tasks shown in it, so the caller can attach checkbox buttons.
type Message struct {
	Text  string
	Tasks []int
}

// Format builds the Telegram HTML message(s) for a meeting with no task done.
func Format(m Meeting, loc *time.Location) []string {
	msgs := FormatTasks(m, nil, loc)
	out := make([]string, len(msgs))
	for i, msg := range msgs {
		out[i] = msg.Text
	}
	return out
}

var taskLineRe = regexp.MustCompile(`(?m)^[☐☑] (\d+)\. `)

// FormatTasks builds the Telegram HTML message(s) for a meeting. It returns
// more than one message only when the content cannot fit in MaxMessageLen.
// Tasks are numbered checkboxes; done (by task number) marks them ☑ and
// strikes them through. loc is used for the date (nil = local time).
func FormatTasks(m Meeting, done map[int]bool, loc *time.Location) []Message {
	texts := format(m, done, loc)
	out := make([]Message, len(texts))
	for i, t := range texts {
		out[i].Text = t
		for _, sub := range taskLineRe.FindAllStringSubmatch(t, -1) {
			n, _ := strconv.Atoi(sub[1])
			out[i].Tasks = append(out[i].Tasks, n)
		}
	}
	return out
}

// TaskCount is the number of checkbox tasks a summary produces.
func TaskCount(summary string) int { return len(Parse(summary).Tasks) }

func format(m Meeting, done map[int]bool, loc *time.Location) []string {
	if loc == nil {
		loc = time.Local
	}
	p := Parse(m.Summary)

	head := []string{"<b>Встреча:</b> " + esc(trunc(orDefault(m.Title, "Без названия"), maxTitle))}
	if !m.Start.IsZero() {
		head = append(head, "<b>Дата:</b> "+m.Start.In(loc).Format("02.01.2006"))
	}
	var foot []string
	if strings.HasPrefix(m.ShareLink, "http://") || strings.HasPrefix(m.ShareLink, "https://") {
		foot = []string{"", "<b>Материалы:</b>", `<a href="` + esc(m.ShareLink) + `">Заметка в Wispr Flow</a>`}
	}

	topics := topicLines(p.Overview, p.Topics)
	decisions := decisionLines(p.Decisions, 0)
	tasks := taskLines(p.Tasks, done)

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

// topicLines renders the recap. When Wispr wrote an intro paragraph it is the
// recap and topics are listed by heading only; otherwise each topic shows its
// first bullet.
func topicLines(overview string, ts []Topic) []string {
	if overview == "" && len(ts) == 0 {
		return nil
	}
	out := []string{"", "<b>Краткие итоги:</b>"}
	if overview != "" {
		out = append(out, esc(trunc(overview, maxOverview)))
		for i, t := range ts {
			if i == maxTopics {
				break
			}
			if t.Heading != "" {
				out = append(out, "• "+esc(trunc(t.Heading, 120)))
			}
		}
		return out
	}
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
// tasks go last. With no assignees at all the list is flat. Tasks are numbered
// in display order and rendered as checkboxes.
func taskLines(ts []Task, done map[int]bool) []string {
	if len(ts) == 0 {
		return nil
	}
	out := []string{"", "<b>Задачи:</b>"}
	var order []string
	groups := map[string][]Task{}
	for _, t := range ts {
		if _, ok := groups[t.Assignee]; !ok {
			order = append(order, t.Assignee)
		}
		groups[t.Assignee] = append(groups[t.Assignee], t)
	}
	n := 0
	bullets := func(group []Task) []string {
		var lines []string
		for _, t := range group {
			n++
			text := esc(trunc(t.Text, maxLine))
			box := "☐"
			if done[n] {
				box, text = "☑", "<s>"+text+"</s>"
			}
			lines = append(lines, fmt.Sprintf("%s %d. %s", box, n, text))
		}
		return lines
	}
	if len(order) == 1 && order[0] == "" {
		return append(out, bullets(groups[""])...)
	}
	for _, a := range order {
		if a != "" {
			out = append(out, "<b>"+esc(trunc(a, 60))+":</b>")
			out = append(out, bullets(groups[a])...)
		}
	}
	if g, ok := groups[""]; ok {
		out = append(out, "<b>Без ответственного:</b>")
		out = append(out, bullets(g)...)
	}
	return out
}

// pack joins head+body+foot into one message, or splits the body by lines
// into several when it exceeds MaxMessageLen. The footer goes last. A
// continuation message repeats the heading it was cut under (e.g. the assignee).
func pack(head, body, foot []string) []string {
	if one := strings.Join(join(head, body, foot), "\n"); utf8.RuneCountInString(one) <= MaxMessageLen {
		return []string{one}
	}
	var msgs []string
	cur := append([]string(nil), head...)
	size := runes(cur)
	lastHeading := ""
	for _, line := range join(body, foot) {
		if size+utf8.RuneCountInString(line)+1 > MaxMessageLen {
			msgs = append(msgs, strings.Join(cur, "\n"))
			cur = []string{"<i>(продолжение)</i>"}
			if lastHeading != "" && !isHeading(line) {
				cur = append(cur, lastHeading)
			}
			size = runes(cur)
		}
		if isHeading(line) {
			lastHeading = line
		}
		cur = append(cur, line)
		size += utf8.RuneCountInString(line) + 1
	}
	return append(msgs, strings.Join(cur, "\n"))
}

// isHeading reports whether a line is one of our "<b>…:</b>" section or group headings.
func isHeading(line string) bool {
	return strings.HasPrefix(line, "<b>") && strings.HasSuffix(line, ":</b>")
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
