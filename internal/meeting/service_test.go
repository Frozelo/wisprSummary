package meeting

import (
	"context"
	"errors"
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
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const goodSummary = "### Принятые решения\n- Делаем X\n\n### Следующие шаги\n- (Иван) Сделать Y\n"

// fakeSource serves meetings newest-modified-first with offset cursors, like Wispr.
type fakeSource struct {
	mu        sync.Mutex
	meetings  map[string]wispr.Meeting
	getCalls  int
	searchErr error
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

func (f *fakeSource) SearchMeetings(_ context.Context, _ time.Time, cursor string, limit int) (wispr.MeetingPage, error) {
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

func (f *fakeSource) GetMeeting(_ context.Context, id string) (wispr.Meeting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	return f.meetings[id], nil
}

type fakeNotifier struct {
	sent []string
	fail func(msg string) error
}

func (n *fakeNotifier) Send(_ context.Context, msg string) error {
	if n.fail != nil {
		if err := n.fail(msg); err != nil {
			return err
		}
	}
	n.sent = append(n.sent, msg)
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
	e.src.meetings["m1"] = withEnd(e.src.meetings["m1"], e.clock.Add(time.Minute)) // ended just now
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

func withEnd(m wispr.Meeting, end time.Time) wispr.Meeting { m.End = end; return m }

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
