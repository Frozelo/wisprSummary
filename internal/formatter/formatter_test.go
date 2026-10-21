package formatter

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const sample = `### Авторизация и JWT
- Обсудили проблемы с **JWT** и время жизни токенов.
- Нужна обратная совместимость.

### Принятые решения
- Использовать Refresh Token.
- Добавить endpoint [обновления](https://x.test) токенов.

### Следующие шаги
- (Иван) Реализовать ` + "`/auth/refresh`" + `.
- (Иван) Добавить интеграционные тесты.
- (Дмитрий) Обновить документацию.
- [ ] Согласовать сроки релиза.
`

func TestParseTasksDecisionsTopics(t *testing.T) {
	p := Parse(sample)

	wantTasks := []Task{
		{"Иван", "Реализовать /auth/refresh."},
		{"Иван", "Добавить интеграционные тесты."},
		{"Дмитрий", "Обновить документацию."},
		{"", "Согласовать сроки релиза."},
	}
	if fmt.Sprint(p.Tasks) != fmt.Sprint(wantTasks) {
		t.Fatalf("tasks = %v\nwant %v", p.Tasks, wantTasks)
	}
	if len(p.Decisions) != 2 || p.Decisions[1] != "Добавить endpoint обновления токенов." {
		t.Fatalf("decisions = %q", p.Decisions)
	}
	if len(p.Topics) != 1 || p.Topics[0].Heading != "Авторизация и JWT" ||
		p.Topics[0].Gist != "Обсудили проблемы с JWT и время жизни токенов." {
		t.Fatalf("topics = %+v", p.Topics)
	}
}

func TestParseHeadingVariants(t *testing.T) {
	for _, h := range []string{"## Дальнейшие шаги", "### Next steps", "# Action Items", "## 6. Следующие шаги"} {
		p := Parse(h + "\n- (A) do it\n")
		if len(p.Tasks) != 1 || p.Tasks[0].Assignee != "A" {
			t.Errorf("%q: tasks = %v", h, p.Tasks)
		}
	}
}

func TestParseEmptyAndUnstructured(t *testing.T) {
	for _, in := range []string{"", "   \n\n", "просто абзац без списков и заголовков"} {
		p := Parse(in)
		if len(p.Tasks)+len(p.Decisions)+len(p.Topics) != 0 {
			t.Errorf("%q: parsed %+v", in, p)
		}
	}
}

func TestParseDoesNotInventTasks(t *testing.T) {
	p := Parse("### Обсуждение\n- Нужно бы сделать X\n- Надо подумать про Y\n")
	if len(p.Tasks) != 0 {
		t.Fatalf("invented tasks: %v", p.Tasks)
	}
}

var start = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

func TestFormatFullMessage(t *testing.T) {
	msgs := Format(Meeting{Title: "Backend API Discussion", Start: start, ShareLink: "https://notes.wisprflow.ai/shared/abc", Summary: sample}, time.UTC)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{
		"<b>Встреча:</b> Backend API Discussion",
		"<b>Дата:</b> 19.10.2026",
		"<b>Краткие итоги:</b>",
		"<b>Принятые решения:</b>",
		"<b>Иван:</b>\n• Реализовать /auth/refresh.\n• Добавить интеграционные тесты.",
		"<b>Дмитрий:</b>",
		"<b>Без ответственного:</b>\n• Согласовать сроки релиза.",
		`<a href="https://notes.wisprflow.ai/shared/abc">Заметка в Wispr Flow</a>`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in:\n%s", want, m)
		}
	}
	if strings.Index(m, "Иван") > strings.Index(m, "Дмитрий") {
		t.Error("assignee order not preserved")
	}
}

func TestFormatEscapesHTML(t *testing.T) {
	msgs := Format(Meeting{
		Title:     "<script>alert(1)</script> & Co",
		Start:     start,
		ShareLink: `https://x.test/?a=1&b="2"`,
		Summary:   "### Следующие шаги\n- (A<B>) fix <b>bold</b> & more\n",
	}, time.UTC)
	m := msgs[0]
	if strings.Contains(m, "<script>") || strings.Contains(m, "fix <b>bold") {
		t.Fatalf("unescaped HTML:\n%s", m)
	}
	for _, want := range []string{"&lt;script&gt;", "&amp; Co", "A&lt;B&gt;", "&lt;b&gt;bold&lt;/b&gt;", "a=1&amp;b=&#34;2&#34;"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in:\n%s", want, m)
		}
	}
}

func TestFormatOmitsEmptySections(t *testing.T) {
	m := Format(Meeting{Title: "T", Start: start, Summary: "### Следующие шаги\n- Сделать X\n"}, time.UTC)[0]
	for _, bad := range []string{"Краткие итоги", "Принятые решения", "Материалы", "Без ответственного"} {
		if strings.Contains(m, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, m)
		}
	}
	if !strings.Contains(m, "<b>Задачи:</b>\n• Сделать X") {
		t.Errorf("flat task list expected:\n%s", m)
	}
}

func TestFormatEmptySummary(t *testing.T) {
	msgs := Format(Meeting{Title: "Quick call", Start: start, ShareLink: "https://x.test/n", Summary: ""}, time.UTC)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Summary пока недоступно") ||
		!strings.Contains(msgs[0], "Quick call") || !strings.Contains(msgs[0], "https://x.test/n") {
		t.Fatalf("msgs = %q", msgs)
	}
}

func TestFormatRejectsNonHTTPLink(t *testing.T) {
	m := Format(Meeting{Title: "T", ShareLink: "javascript:alert(1)", Summary: sample}, time.UTC)[0]
	if strings.Contains(m, "javascript:") {
		t.Fatalf("unsafe link rendered:\n%s", m)
	}
}

func TestFormatSplitsLongMessages(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("### Следующие шаги\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "- (Иван) Задача номер %d: %s\n", i, strings.Repeat("слово ", 20))
	}
	msgs := Format(Meeting{Title: "Big", Start: start, ShareLink: "https://x.test/n", Summary: sb.String()}, time.UTC)
	if len(msgs) < 2 {
		t.Fatalf("expected a split, got %d message(s)", len(msgs))
	}
	var all string
	for i, m := range msgs {
		if n := utf8.RuneCountInString(m); n > MaxMessageLen {
			t.Errorf("message %d has %d runes", i, n)
		}
		all += m
	}
	if !strings.Contains(all, "Задача номер 119") || !strings.HasSuffix(msgs[len(msgs)-1], "Заметка в Wispr Flow</a>") {
		t.Error("tasks lost or link not in the last message")
	}
}
