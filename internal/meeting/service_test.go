package meeting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Frozelo/wisprSummary/internal/storage"
	"github.com/Frozelo/wisprSummary/internal/telegram"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const goodSummary = "### Принятые решения\n- Делаем X\n\n### Следующие шаги\n- (Иван) Сделать Y\n"

// fakeSource serves meetings newest-modified-first with offset cursors, like Wispr.
type fakeSource struct {
	mu         sync.Mutex
	meetings   map[string]wispr.Meeting
	getCalls   int
	searchErr  error
	accountErr error
}

func newSource() *fakeSource { return &fakeSource{meetings: map[string]wispr.Meeting{}} }

func (f *fakeSource) add(id string, modified time.Time, finalized bool, summary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meetings[id] = wispr.Meeting{
		MeetingInfo: wispr.MeetingInfo{ID: id, Title: "Meeting " + id, Finalized: finalized,
			Start: modified.Add(-time.Hour), End: modified.Add(-time.Minute), ModifiedAt: modified,
			ShareLink: "https://notes.example/" + id},
		Summary: summary,
	}
}

func (f *fakeSource) SearchMeetings(_ context.Context, cursor string, limit int) (wispr.MeetingPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.searchErr != nil {
		return wispr.MeetingPage{}, f.searchErr
	}
	var all []wispr.MeetingInfo
	for _, m := range f.meetings {
		all = append(all, m.MeetingInfo)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ModifiedAt.After(all[j].ModifiedAt) })
	off, _ := strconv.Atoi(cursor)
	end := min(off+limit, len(all))
	page := wispr.MeetingPage{Meetings: all[off:end]}
	if end < len(all) {
		page.HasMore, page.NextCursor = true, strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeSource) AccountName(context.Context) (string, error) {
	if f.accountErr != nil {
		return "", f.accountErr
	}
	return "ONIPEDE CECILIA 1910005257", nil
}

func (f *fakeSource) GetMeeting(_ context.Context, id string) (wispr.Meeting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.meetings[id], nil
}

type fakeNotifier struct {
	sent  []string
	kbs   []telegram.Keyboard
	edits []edit
	fail  func(msg string) error
}

type edit struct {
	id   int64
	text string
	kb   telegram.Keyboard
}

func (n *fakeNotifier) Send(_ context.Context, msg string, kb telegram.Keyboard) (int64, error) {
	if n.fail != nil {
		if err := n.fail(msg); err != nil {
			return 0, err
		}
	}
	n.sent = append(n.sent, msg)
	n.kbs = append(n.kbs, kb)
	return int64(100 + len(n.sent)), nil
}

func (n *fakeNotifier) Edit(_ context.Context, id int64, text string, kb telegram.Keyboard) error {
	n.edits = append(n.edits, edit{id, text, kb})
	return nil
}

type env struct {
	src    *fakeSource
	n      *fakeNotifier
	st     *storage.SQLite
	svc    *Service
	clock  time.Time
	dbPath string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{src: newSource(), n: &fakeNotifier{}, clock: t0, dbPath: filepath.Join(t.TempDir(), "state.db")}
	e.open(t)
	return e
}

// open (re)creates the store and a fresh Service, like a process restart.
func (e *env) open(t *testing.T) {
	t.Helper()
	if e.st != nil {
		e.st.Close()
	}
	st, err := storage.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.svc = NewService(Config{Interval: 2 * time.Minute, PageSize: 2, Loc: time.UTC}, e.src, e.n, st,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.svc.now = func() time.Time { return e.clock }
}

func (e *env) tick(t *testing.T, advance time.Duration) {
	t.Helper()
	e.clock = e.clock.Add(advance)
	if err := e.svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func TestFirstRunDoesNotReplayHistory(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.src.add("old"+strconv.Itoa(i), t0.Add(-time.Duration(i+1)*time.Hour), true, goodSummary)
	}
	e.tick(t, 0)
	if len(e.n.sent) != 0 {
		t.Fatalf("first run sent %d messages", len(e.n.sent))
	}
	// A meeting finished afterwards is delivered.
	e.src.add("new", t0.Add(time.Minute), true, goodSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 || !strings.Contains(e.n.sent[0], "Meeting new") {
		t.Fatalf("sent = %q", e.n.sent)
	}
}

func TestNoDuplicates(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, goodSummary)
	for i := 0; i < 4; i++ {
		e.tick(t, 2*time.Minute)
	}
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(e.n.sent))
	}
	if e.src.getCalls != 1 {
		t.Fatalf("get_meeting called %d times, want 1", e.src.getCalls)
	}
}

func TestUnfinalizedWaitsThenSends(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), false, goodSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 0 {
		t.Fatal("sent an unfinalized meeting")
	}
	e.src.add("m1", e.clock.Add(time.Minute), true, goodSummary) // finalized -> modified again
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(e.n.sent))
	}
}

func TestEmptySummary(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, "  \n")
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 0 {
		t.Fatal("must wait for the summary within the grace period")
	}
	if done, _ := e.st.IsProcessed(context.Background(), "m1"); done {
		t.Fatal("marked processed too early")
	}
	e.tick(t, 15*time.Minute) // grace over: skipped, not sent, never retried
	if len(e.n.sent) != 0 {
		t.Fatalf("sent %q for empty summary", e.n.sent)
	}
	if done, _ := e.st.IsProcessed(context.Background(), "m1"); !done {
		t.Fatal("empty meeting should be marked processed")
	}
}

// Wispr can finalize a long meeting well after it ended; the summary grace must
// count from when it became final, not from the end time.
func TestLateSummaryIsNotDropped(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	mod := t0.Add(time.Minute)
	e.src.add("late", mod, true, "")
	m := e.src.meetings["late"]
	m.End = mod.Add(-40 * time.Minute) // ended 40 minutes before it was finalized
	e.src.meetings["late"] = m

	e.tick(t, 2*time.Minute)
	if done, _ := e.st.IsProcessed(context.Background(), "late"); done {
		t.Fatal("dropped a meeting whose summary is still being generated")
	}
	e.src.add("late", e.clock.Add(time.Minute), true, goodSummary) // summary arrives
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(e.n.sent))
	}
}

func TestEditedOldMeetingIsNotNews(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("oldie", t0.Add(time.Minute), true, goodSummary) // renamed today...
	m := e.src.meetings["oldie"]
	m.Start, m.End = t0.Add(-130*time.Hour), t0.Add(-129*time.Hour) // ...but held 5 days ago
	e.src.meetings["oldie"] = m
	e.src.add("fresh", t0.Add(time.Minute), true, goodSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 || !strings.Contains(e.n.sent[0], "Meeting fresh") {
		t.Fatalf("sent = %q", e.n.sent)
	}
	if done, _ := e.st.IsProcessed(context.Background(), "oldie"); !done {
		t.Fatal("old meeting should be recorded so it is not reconsidered")
	}
}

func longSummary() string {
	var sb strings.Builder
	sb.WriteString("### Следующие шаги\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "- (Иван) Задача %d: %s\n", i, strings.Repeat("слово ", 20))
	}
	return sb.String()
}

func TestMultiPartRetryDoesNotRepeatDeliveredParts(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("big", t0.Add(time.Minute), true, longSummary())
	calls, failSecond := 0, true
	e.n.fail = func(string) error {
		calls++
		if calls == 2 && failSecond {
			failSecond = false
			return errors.New("telegram hiccup")
		}
		return nil
	}
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("after the failure sent = %d, want 1", len(e.n.sent))
	}
	e.tick(t, 5*time.Minute) // backoff over
	parts := len(e.n.sent)
	seen := map[string]bool{}
	for _, m := range e.n.sent {
		if seen[m] {
			t.Fatal("a part was delivered twice")
		}
		seen[m] = true
	}
	if parts < 2 {
		t.Fatalf("expected several parts, got %d", parts)
	}
	if done, _ := e.st.IsProcessed(context.Background(), "big"); !done {
		t.Fatal("not marked processed after the last part")
	}
}

func TestClaimedMeetingIsLeftToTheOtherInstance(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, goodSummary)
	ok, err := e.st.Claim(context.Background(), "m1", e.clock, claimTTL) // "another instance"
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 0 {
		t.Fatal("sent a meeting claimed by another instance")
	}
	e.tick(t, 15*time.Minute) // that instance died: claim expired
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d after the claim expired, want 1", len(e.n.sent))
	}
}

func TestBackoffGrowsButStaysBounded(t *testing.T) {
	e := newEnv(t)
	info := wispr.MeetingInfo{ID: "x"}
	for i := 0; i < 200; i++ {
		e.svc.recordFailure(info, errors.New("boom"))
		d := e.svc.failures["x"].next.Sub(e.clock)
		if d <= 0 || d > maxBackoff {
			t.Fatalf("failure %d: backoff %v out of (0, %v]", i+1, d, maxBackoff)
		}
	}
}

func TestStateForMeetingsOutOfWindowIsPruned(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.svc.failures["gone"] = &failure{n: 1, next: e.clock}
	e.svc.sent["gone"] = 1
	e.tick(t, 2*time.Minute)
	if len(e.svc.failures) != 0 || len(e.svc.sent) != 0 {
		t.Fatalf("leaked state: %v %v", e.svc.failures, e.svc.sent)
	}
}

type countingNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (n *countingNotifier) Send(_ context.Context, m string, _ telegram.Keyboard) (int64, error) {
	n.mu.Lock()
	n.msgs = append(n.msgs, m)
	n.mu.Unlock()
	return 1, nil
}

func (n *countingNotifier) Edit(context.Context, int64, string, telegram.Keyboard) error { return nil }

func (n *countingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.msgs)
}

func TestRunAlertsOnceOnLostLoginAndStopsOnCancel(t *testing.T) {
	e := newEnv(t)
	cn := &countingNotifier{}
	e.svc.notify = cn
	e.svc.cfg.Interval = 5 * time.Millisecond
	e.src.searchErr = fmt.Errorf("initialize: %w", wispr.ErrLoginRequired)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for cn.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // several more failing ticks
	if cn.count() != 1 {
		t.Fatalf("alerts = %d, want exactly 1", cn.count())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestShutdownAfterSendStillRecordsMeeting(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, goodSummary)
	ctx, cancel := context.WithCancel(context.Background())
	e.n.fail = func(string) error { return nil }
	n := e.n
	e.svc.notify = notifierFunc(func(c context.Context, msg string) error {
		_, err := n.Send(c, msg, nil)
		cancel() // SIGTERM arrives right after the message went out
		return err
	})
	e.clock = e.clock.Add(2 * time.Minute)
	_ = e.svc.Tick(ctx)
	if len(n.sent) != 1 {
		t.Fatalf("sent %d", len(n.sent))
	}
	if done, _ := e.st.IsProcessed(context.Background(), "m1"); !done {
		t.Fatal("delivered meeting was not recorded; it would be resent after restart")
	}
}

type notifierFunc func(context.Context, string) error

func (f notifierFunc) Send(ctx context.Context, m string, _ telegram.Keyboard) (int64, error) {
	return 1, f(ctx, m)
}

func (f notifierFunc) Edit(context.Context, int64, string, telegram.Keyboard) error { return nil }
func TestSendFailureRetriesAndDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("bad", t0.Add(time.Minute), true, goodSummary)
	e.src.add("good", t0.Add(2*time.Minute), true, goodSummary)
	failing := true
	e.n.fail = func(msg string) error {
		if failing && strings.Contains(msg, "Meeting bad") {
			return errors.New("telegram down")
		}
		return nil
	}

	e.tick(t, 3*time.Minute)
	if len(e.n.sent) != 1 || !strings.Contains(e.n.sent[0], "Meeting good") {
		t.Fatalf("one failure must not block others: %q", e.n.sent)
	}
	if done, _ := e.st.IsProcessed(context.Background(), "bad"); done {
		t.Fatal("failed meeting must stay unprocessed")
	}

	e.tick(t, time.Minute) // still inside backoff: not retried
	if len(e.n.sent) != 1 {
		t.Fatal("retried during backoff")
	}
	failing = false
	e.tick(t, 5*time.Minute) // backoff over, Telegram back
	if len(e.n.sent) != 2 || !strings.Contains(e.n.sent[1], "Meeting bad") {
		t.Fatalf("sent = %q", e.n.sent)
	}
}

func TestRestartRecovery(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, goodSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d", len(e.n.sent))
	}

	// Restart: new process, same database. Nothing is resent and it is not a "first run".
	e.open(t)
	e.src.add("old-late", t0.Add(-3*time.Hour), true, goodSummary) // history must stay ignored
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("restart resent: %q", e.n.sent)
	}

	// Downtime: meetings piled up across several pages are all delivered after restart.
	for i := 0; i < 5; i++ {
		e.src.add("down"+strconv.Itoa(i), e.clock.Add(time.Duration(i+1)*time.Minute), true, goodSummary)
	}
	e.open(t)
	e.tick(t, 30*time.Minute)
	if len(e.n.sent) != 6 {
		t.Fatalf("after downtime sent %d, want 6", len(e.n.sent))
	}
}

func TestSharedMeetingsAreSkipped(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("theirs", t0.Add(time.Minute), true, goodSummary)
	m := e.src.meetings["theirs"]
	m.Shared = true
	e.src.meetings["theirs"] = m
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 0 {
		t.Fatal("sent someone else's meeting")
	}
}

func TestListErrorKeepsWatermark(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	before, _, _ := e.st.Watermark(context.Background())
	e.src.searchErr = errors.New("wispr down")
	e.clock = e.clock.Add(time.Hour)
	if err := e.svc.Tick(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	after, _, _ := e.st.Watermark(context.Background())
	if !after.Equal(before) {
		t.Fatalf("watermark moved on error: %v -> %v", before, after)
	}
}

func TestRunOnceAlertsOnlyOnceAcrossProcesses(t *testing.T) {
	e := newEnv(t)
	cn := &countingNotifier{}
	e.svc.notify = cn
	e.src.searchErr = fmt.Errorf("initialize: %w", wispr.ErrLoginRequired)

	for i := 0; i < 3; i++ { // three cron runs, each a fresh process on the same state
		e.open(t)
		e.svc.notify = cn
		if err := e.svc.RunOnce(context.Background()); err != nil {
			t.Fatalf("login loss must not be an error exit: %v", err)
		}
	}
	if cn.count() != 1 {
		t.Fatalf("alerts = %d, want 1", cn.count())
	}

	e.src.searchErr = nil // user logged in again
	e.open(t)
	e.svc.notify = cn
	if err := e.svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.src.searchErr = fmt.Errorf("initialize: %w", wispr.ErrLoginRequired)
	e.open(t)
	e.svc.notify = cn
	e.svc.RunOnce(context.Background())
	if cn.count() != 2 {
		t.Fatalf("alert after a new login loss: alerts = %d, want 2", cn.count())
	}
}

func TestRunOnceReturnsOtherErrors(t *testing.T) {
	e := newEnv(t)
	e.src.searchErr = errors.New("wispr 502")
	if err := e.svc.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the error so the CI job is marked failed")
	}
}

func TestPartialDeliverySurvivesProcessRestart(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("big", t0.Add(time.Minute), true, longSummary())
	calls := 0
	e.n.fail = func(string) error {
		calls++
		if calls == 2 {
			return errors.New("telegram hiccup")
		}
		return nil
	}
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d", len(e.n.sent))
	}
	e.open(t) // next cron run: new process, in-memory state gone
	e.tick(t, 5*time.Minute)
	seen := map[string]bool{}
	for _, m := range e.n.sent {
		if seen[m] {
			t.Fatal("a part was delivered twice after restart")
		}
		seen[m] = true
	}
	if done, _ := e.st.IsProcessed(context.Background(), "big"); !done {
		t.Fatal("not completed after restart")
	}
}

const accountSummary = `Тестовая встреча: ONIPEDE CECILIA 1910005257 распределил задачи.

### Распределение задач
- ONIPEDE CECILIA 1910005257 берёт бэкенд-часть.

### Следующие шаги
- (Антон) Сделать дизайн
`

func TestSelfNameReplacesWisprAccountName(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.SelfName = "Иван"
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, accountSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d", len(e.n.sent))
	}
	m := e.n.sent[0]
	if strings.Contains(m, "ONIPEDE") || !strings.Contains(m, "Иван распределил задачи") {
		t.Fatalf("account name not replaced:\n%s", m)
	}
}

func TestSelfNameUnsetKeepsTextAndAccountErrorDoesNotBlock(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, accountSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 || !strings.Contains(e.n.sent[0], "ONIPEDE") {
		t.Fatalf("text must be untouched without SELF_NAME: %q", e.n.sent)
	}

	e.svc.cfg.SelfName = "Иван"
	e.src.accountErr = errors.New("wispr down")
	e.src.add("m2", e.clock.Add(time.Minute), true, accountSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 2 {
		t.Fatalf("a failing account lookup must not block delivery, sent %d", len(e.n.sent))
	}
}

// ---- task checkboxes ----

const tasksSummary = "### Следующие шаги\n- (Иван) Первая\n- (Иван) Вторая\n- (Дима) Третья\n"

const chatID = int64(42)

type fakeCallbacks struct {
	pending []telegram.Update // all updates ever received
	answers []string
}

func (f *fakeCallbacks) Updates(_ context.Context, offset int64, _ int) ([]telegram.Update, error) {
	var out []telegram.Update
	for _, u := range f.pending {
		if u.UpdateID >= offset {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeCallbacks) Answer(_ context.Context, _ string, text string) error {
	f.answers = append(f.answers, text)
	return nil
}

func press(updateID int64, chat, msgID int64, data string) telegram.Update {
	cq := &telegram.CallbackQuery{ID: fmt.Sprint("cq", updateID), Data: data}
	cq.Message = &struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{MessageID: msgID}
	cq.Message.Chat.ID = chat
	return telegram.Update{UpdateID: updateID, CallbackQuery: cq}
}

// delivered sends one meeting with tasks and returns its digest id.
func delivered(t *testing.T, e *env, cb *fakeCallbacks) int64 {
	t.Helper()
	e.svc.cfg.ChatID = chatID
	e.svc.EnableCallbacks(cb)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, tasksSummary)
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 {
		t.Fatalf("sent %d", len(e.n.sent))
	}
	id, err := e.st.SaveDigest(context.Background(), storage.Digest{MeetingID: "m1"}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeliveredMessageHasCheckboxButtons(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	id := delivered(t, e, cb)
	kb := e.n.kbs[0]
	if len(kb) != 1 || len(kb[0]) != 3 {
		t.Fatalf("keyboard = %+v", kb)
	}
	if kb[0][0].Text != "☐1" || kb[0][0].Data != fmt.Sprintf("t:%d:1", id) {
		t.Fatalf("button = %+v", kb[0][0])
	}
	if !strings.Contains(e.n.sent[0], "☐ 1. Первая") {
		t.Fatalf("text = %s", e.n.sent[0])
	}
}

func TestPressTogglesTaskAndRedrawsMessage(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	id := delivered(t, e, cb)
	data := fmt.Sprintf("t:%d:2", id)

	cb.pending = append(cb.pending, press(1, chatID, 101, data))
	if err := e.svc.HandleUpdates(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if len(e.n.edits) != 1 {
		t.Fatalf("edits = %d", len(e.n.edits))
	}
	ed := e.n.edits[0]
	if ed.id != 101 || !strings.Contains(ed.text, "☑ 2. <s>Вторая</s>") || !strings.Contains(ed.text, "☐ 1. Первая") {
		t.Fatalf("edit = %+v", ed)
	}
	if ed.kb[0][1].Text != "☑2" || ed.kb[0][0].Text != "☐1" {
		t.Fatalf("keyboard = %+v", ed.kb)
	}
	if cb.answers[0] != "✓ Выполнено" {
		t.Fatalf("answers = %q", cb.answers)
	}

	// A second press on the same task un-ticks it; the stored offset prevents replays.
	cb.pending = append(cb.pending, press(2, chatID, 101, data))
	if err := e.svc.HandleUpdates(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleUpdates(context.Background(), 0); err != nil { // nothing new
		t.Fatal(err)
	}
	if len(e.n.edits) != 2 || !strings.Contains(e.n.edits[1].text, "☐ 2. Вторая") || strings.Contains(e.n.edits[1].text, "<s>") {
		t.Fatalf("edits = %+v", e.n.edits)
	}
	if cb.answers[1] != "↩ Снова в работе" {
		t.Fatalf("answers = %q", cb.answers)
	}
}

func TestPressesSurviveRestartWithoutReplay(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	id := delivered(t, e, cb)
	cb.pending = append(cb.pending, press(1, chatID, 101, fmt.Sprintf("t:%d:1", id)))
	e.svc.HandleUpdates(context.Background(), 0)

	e.open(t) // restart: new process, same database
	e.svc.cfg.ChatID = chatID
	e.svc.EnableCallbacks(cb)
	e.svc.HandleUpdates(context.Background(), 0)
	done, _ := e.st.DoneTasks(context.Background(), id)
	if !done[1] || len(e.n.edits) != 1 {
		t.Fatalf("a press was replayed after restart: done=%v edits=%d", done, len(e.n.edits))
	}
}

func TestPressFromForeignChatOrBadDataIsIgnored(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	id := delivered(t, e, cb)
	good := fmt.Sprintf("t:%d:1", id)
	cb.pending = []telegram.Update{
		press(1, 999, 101, good),                                                   // someone else's chat
		press(2, chatID, 101, "garbage"),                                           // malformed
		press(3, chatID, 101, fmt.Sprintf("t:%d:9", id)),                           // task out of range
		press(4, chatID, 101, "t:abc:1"),                                           // not a number
		{UpdateID: 5, CallbackQuery: &telegram.CallbackQuery{ID: "x", Data: good}}, // no message
	}
	if err := e.svc.HandleUpdates(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if done, _ := e.st.DoneTasks(context.Background(), id); len(done) != 0 || len(e.n.edits) != 0 {
		t.Fatalf("ignored presses changed state: done=%v edits=%d", done, len(e.n.edits))
	}
}

func TestPressOnUnknownDigestSaysStale(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	delivered(t, e, cb)
	cb.pending = []telegram.Update{press(1, chatID, 101, "t:9999:1")}
	e.svc.HandleUpdates(context.Background(), 0)
	if len(cb.answers) != 1 || cb.answers[0] != "Список устарел" || len(e.n.edits) != 0 {
		t.Fatalf("answers=%q edits=%d", cb.answers, len(e.n.edits))
	}
}

func TestRunOnceAppliesPendingPresses(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	id := delivered(t, e, cb)
	cb.pending = []telegram.Update{press(1, chatID, 101, fmt.Sprintf("t:%d:3", id))}
	if err := e.svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done, _ := e.st.DoneTasks(context.Background(), id); !done[3] {
		t.Fatalf("pending press not applied in one-shot mode: %v", done)
	}
}

func TestPressEditsTheMessageHoldingTheTask(t *testing.T) {
	e := newEnv(t)
	cb := &fakeCallbacks{}
	e.svc.cfg.ChatID = chatID
	e.svc.EnableCallbacks(cb)
	e.tick(t, 0)
	e.src.add("big", t0.Add(time.Minute), true, longSummary())
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) < 2 {
		t.Fatalf("expected a multi-part delivery, got %d", len(e.n.sent))
	}
	id, _ := e.st.SaveDigest(context.Background(), storage.Digest{MeetingID: "big"}, e.clock)
	last := len(e.n.sent) - 1
	lastTasks := e.n.kbs[last]
	data := lastTasks[0][0].Data                                // first task shown in the last part
	cb.pending = []telegram.Update{press(1, chatID, 101, data)} // pressed message id is irrelevant here
	e.svc.HandleUpdates(context.Background(), 0)
	if len(e.n.edits) != 1 || e.n.edits[0].id != int64(100+last+1) {
		t.Fatalf("edit went to %+v, want message %d of part %d (digest %d)", e.n.edits, 100+last+1, last, id)
	}
}

func TestNoTasksNoButtons(t *testing.T) {
	e := newEnv(t)
	e.tick(t, 0)
	e.src.add("m1", t0.Add(time.Minute), true, "### Принятые решения\n- Делаем X\n")
	e.tick(t, 2*time.Minute)
	if len(e.n.sent) != 1 || e.n.kbs[0] != nil {
		t.Fatalf("keyboard for a meeting without tasks: %+v", e.n.kbs)
	}
}
