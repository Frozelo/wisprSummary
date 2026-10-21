// Package meeting polls Wispr for finished meetings and delivers each one to
// Telegram exactly once (best effort: a crash between send and record can
// repeat one message).
package meeting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Frozelo/wisprSummary/internal/formatter"
	"github.com/Frozelo/wisprSummary/internal/storage"
	"github.com/Frozelo/wisprSummary/internal/telegram"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

const (
	maxPages   = 20 // Wispr caps paging at 1000 results per query
	claimTTL   = 10 * time.Minute
	maxBackoff = 30 * time.Minute
	dbTimeout  = 5 * time.Second
	// Checkboxes of messages older than this stop working.
	digestRetention = 60 * 24 * time.Hour
)

// Source is the Wispr side (implemented by *wispr.Client).
type Source interface {
	SearchMeetings(ctx context.Context, cursor string, limit int) (wispr.MeetingPage, error)
	GetMeeting(ctx context.Context, id string) (wispr.Meeting, error)
	AccountName(ctx context.Context) (string, error)
}

// Notifier delivers HTML messages to the owner's chat and edits them later.
type Notifier interface {
	Send(ctx context.Context, html string, kb telegram.Keyboard) (messageID int64, err error)
	Edit(ctx context.Context, messageID int64, html string, kb telegram.Keyboard) error
}

// Callbacks reads button presses. It is optional: without it task checkboxes
// are shown but pressing them does nothing.
type Callbacks interface {
	Updates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error)
	Answer(ctx context.Context, callbackID, text string) error
}

type Store interface {
	IsProcessed(ctx context.Context, id string) (bool, error)
	MarkProcessed(ctx context.Context, id, title string, at time.Time) error
	Claim(ctx context.Context, id string, now time.Time, ttl time.Duration) (bool, error)
	Release(ctx context.Context, id string) error
	Watermark(ctx context.Context) (time.Time, bool, error)
	SetWatermark(ctx context.Context, t time.Time) error
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
	DeleteKV(ctx context.Context, key string) error

	SaveDigest(ctx context.Context, d storage.Digest, now time.Time) (int64, error)
	DigestByID(ctx context.Context, id int64) (storage.Digest, bool, error)
	SaveDigestMessage(ctx context.Context, digestID int64, part int, messageID int64) error
	DigestMessages(ctx context.Context, digestID int64) (map[int]int64, error)
	DoneTasks(ctx context.Context, digestID int64) (map[int]bool, error)
	ToggleTask(ctx context.Context, digestID int64, task int) (bool, error)
	PruneDigests(ctx context.Context, cutoff time.Time) error
}

type Config struct {
	Interval     time.Duration // poll period
	Lookback     time.Duration // longest a pending/failed meeting is retried
	Backfill     time.Duration // on first run, also send meetings modified this recently
	Slack        time.Duration // overlap kept when advancing the watermark
	SummaryGrace time.Duration // wait this long after a meeting ends for its summary
	PageSize     int
	Loc          *time.Location
	// ChatID is the only chat whose button presses are honoured.
	ChatID int64
	// SelfName replaces the Wispr account name in titles and summaries (Wispr
	// writes the account owner's display name where the speaker said "I").
	SelfName string
}

func (c *Config) setDefaults() {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Minute
	}
	if c.Lookback <= 0 {
		c.Lookback = 48 * time.Hour
	}
	if c.Slack <= 0 {
		c.Slack = 10 * time.Minute
	}
	if c.SummaryGrace <= 0 {
		c.SummaryGrace = 10 * time.Minute
	}
	if c.PageSize <= 0 {
		c.PageSize = 50
	}
}

type failure struct {
	n    int
	next time.Time
}

type Service struct {
	cfg      Config
	src      Source
	notify   Notifier
	store    Store
	log      *slog.Logger
	now      func() time.Time
	failures map[string]*failure
	sent     map[string]int // meeting id -> message parts already delivered
	account  string         // cached Wispr account name
	cb       Callbacks
}

func NewService(cfg Config, src Source, n Notifier, st Store, log *slog.Logger) *Service {
	cfg.setDefaults()
	return &Service{cfg: cfg, src: src, notify: n, store: st, log: log, now: time.Now, failures: map[string]*failure{}, sent: map[string]int{}}
}

// EnableCallbacks turns on handling of task checkbox presses.
func (s *Service) EnableCallbacks(cb Callbacks) { s.cb = cb }

// Run polls until ctx is cancelled. Errors never stop the loop; a lost Wispr
// login triggers a single Telegram alert until polling works again.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	if s.cb != nil { // button presses are handled immediately, apart from the poll cadence
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.listen(ctx)
		}()
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		s.handleTick(ctx, s.Tick(ctx))
		t.Reset(s.cfg.Interval)
	}
}

// RunOnce performs a single poll cycle, for schedulers that start a fresh
// process per run (CI cron). A lost login is reported to Telegram once and is
// not an error exit; any other failure is returned.
func (s *Service) RunOnce(ctx context.Context) error {
	// Presses made since the previous run (Telegram keeps updates for 24h).
	if err := s.HandleUpdates(ctx, 0); err != nil && ctx.Err() == nil {
		s.log.Error("handle button presses", "err", err)
	}
	err := s.Tick(ctx)
	s.handleTick(ctx, err)
	if err != nil && !errors.Is(err, wispr.ErrLoginRequired) {
		return err
	}
	return nil
}

const loginAlertKey = "login_alert"

// handleTick logs a tick's outcome and manages the "login expired" alert. The
// alert flag lives in the store so it survives restarts and one-shot runs.
func (s *Service) handleTick(ctx context.Context, err error) {
	switch {
	case err == nil:
		if _, ok, _ := s.store.GetKV(ctx, loginAlertKey); ok {
			_ = s.store.DeleteKV(ctx, loginAlertKey)
		}
	case ctx.Err() != nil:
	case errors.Is(err, wispr.ErrLoginRequired):
		s.log.Error("wispr login required; run `bot login`")
		if _, ok, _ := s.store.GetKV(ctx, loginAlertKey); !ok {
			if _, err := s.notify.Send(ctx, "⚠️ Wispr login expired. Run <code>bot login</code> to resume summaries.", nil); err == nil {
				_ = s.store.SetKV(ctx, loginAlertKey, "1")
			}
		}
	default:
		s.log.Error("poll failed", "err", err)
	}
}

// Tick performs one poll cycle.
func (s *Service) Tick(ctx context.Context) error {
	start := s.now()
	wm, ok, err := s.store.Watermark(ctx)
	if err != nil {
		return fmt.Errorf("read watermark: %w", err)
	}
	if !ok { // first run: do not replay history
		wm = start.Add(-s.cfg.Backfill)
		if err := s.store.SetWatermark(ctx, wm); err != nil {
			return err
		}
		s.log.Info("first run: only meetings modified after now are sent", "since", wm)
	}

	found, err := s.collect(ctx, wm)
	if err != nil {
		return err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ModifiedAt.Before(found[j].ModifiedAt) })

	var oldestPending time.Time
	hold := func(m wispr.MeetingInfo) {
		if oldestPending.IsZero() || m.ModifiedAt.Before(oldestPending) {
			oldestPending = m.ModifiedAt
		}
	}
	seen := make(map[string]bool, len(found))
	for _, m := range found {
		seen[m.ID] = true
		if m.Shared {
			continue // someone else's notes
		}
		done, err := s.store.IsProcessed(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("check processed: %w", err)
		}
		if done {
			continue
		}
		if s.tooOld(m, start) {
			// An old meeting that was merely edited (renamed, moved) bumps modified_at;
			// it is not news. Record it so it is never reconsidered.
			if err := s.markProcessed(ctx, m); err != nil {
				return err
			}
			continue
		}
		if !m.Finalized {
			hold(m)
			continue
		}
		if f := s.failures[m.ID]; f != nil && s.now().Before(f.next) {
			hold(m)
			continue
		}
		claimed, err := s.store.Claim(ctx, m.ID, s.now(), claimTTL)
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		if !claimed {
			continue // another instance is delivering it
		}
		switch err := s.process(ctx, m); {
		case err == nil:
			delete(s.failures, m.ID)
			delete(s.sent, m.ID)
		case ctx.Err() != nil:
			s.release(m.ID)
			return ctx.Err()
		default:
			s.release(m.ID)
			s.recordFailure(m, err)
			hold(m)
		}
	}
	// Forget state for meetings that left the scan window.
	for id := range s.failures {
		if !seen[id] {
			delete(s.failures, id)
		}
	}
	for id := range s.sent {
		if !seen[id] {
			delete(s.sent, id)
		}
	}

	if err := s.store.PruneDigests(ctx, start.Add(-digestRetention)); err != nil {
		s.log.Error("prune old digests", "err", err)
	}

	// Advance the watermark, but keep retrying pending meetings up to Lookback.
	next := start.Add(-s.cfg.Slack)
	if !oldestPending.IsZero() && oldestPending.Before(next) {
		next = oldestPending
	}
	if floor := start.Add(-s.cfg.Lookback); next.Before(floor) {
		next = floor
	}
	if next.After(wm) {
		return s.store.SetWatermark(ctx, next)
	}
	return nil
}

// collect pages through results (newest modified first) down to the watermark.
func (s *Service) collect(ctx context.Context, wm time.Time) ([]wispr.MeetingInfo, error) {
	var out []wispr.MeetingInfo
	cursor := ""
	for range maxPages {
		page, err := s.src.SearchMeetings(ctx, cursor, s.cfg.PageSize)
		if err != nil {
			return nil, err
		}
		if page.Skipped > 0 {
			s.log.Warn("skipped undecodable meetings in search results", "count", page.Skipped)
		}
		reached := false
		for _, m := range page.Meetings {
			if m.ModifiedAt.Before(wm) {
				reached = true
				continue
			}
			out = append(out, m)
		}
		if reached || !page.HasMore || page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
	s.log.Warn("page limit reached while scanning meetings", "pages", maxPages)
	return out, nil
}

var errSummaryPending = errors.New("summary not ready yet")

func (s *Service) process(ctx context.Context, info wispr.MeetingInfo) error {
	m, err := s.src.GetMeeting(ctx, info.ID)
	if err != nil {
		return fmt.Errorf("get meeting: %w", err)
	}
	m.Title, m.Summary = s.applySelfName(ctx, m.Title), s.applySelfName(ctx, m.Summary)
	if strings.TrimSpace(m.Summary) == "" {
		// Wait for the summary relative to when the meeting became final
		// (modified_at), not when it ended: processing can lag the end by far more than the grace.
		ready := m.ModifiedAt
		if m.End.After(ready) {
			ready = m.End
		}
		if s.now().Sub(ready) < s.cfg.SummaryGrace {
			return errSummaryPending
		}
		s.log.Info("skipping meeting without summary", "id", info.ID)
		return s.markProcessed(ctx, info)
	}
	fm := formatter.Meeting{Title: m.Title, Start: m.Start, ShareLink: m.ShareLink, Summary: m.Summary}
	if err := s.Deliver(ctx, info.ID, fm); err != nil {
		return err
	}
	if err := s.markProcessed(ctx, info); err != nil {
		return fmt.Errorf("mark processed (message was sent): %w", err)
	}
	s.log.Info("meeting sent", "id", info.ID)
	return nil
}

// applySelfName swaps the Wispr account name for the configured SelfName.
func (s *Service) applySelfName(ctx context.Context, text string) string {
	if s.cfg.SelfName == "" || text == "" {
		return text
	}
	if s.account == "" {
		name, err := s.src.AccountName(ctx)
		if err != nil || name == "" {
			s.log.Warn("could not read the Wispr account name; SELF_NAME not applied", "err", err)
			return text
		}
		s.account = name
	}
	return strings.ReplaceAll(text, s.account, s.cfg.SelfName)
}

// partsSent returns how many message parts of a meeting were already delivered
// (kept in the store so one-shot runs and restarts also resume mid-meeting).
func (s *Service) partsSent(ctx context.Context, id string) int {
	if n, ok := s.sent[id]; ok {
		return n
	}
	if v, ok, _ := s.store.GetKV(ctx, "parts:"+id); ok {
		n, _ := strconv.Atoi(v)
		s.sent[id] = n
		return n
	}
	return 0
}

func (s *Service) setPartsSent(ctx context.Context, id string, n int) {
	s.sent[id] = n
	if err := s.store.SetKV(ctx, "parts:"+id, strconv.Itoa(n)); err != nil {
		s.log.Error("persist delivered parts", "id", id, "err", err)
	}
}

// Deliver formats a meeting and sends it with task checkboxes, resuming after
// the parts already delivered for id. The meeting's digest is stored so the
// checkboxes keep working.
func (s *Service) Deliver(ctx context.Context, id string, fm formatter.Meeting) error {
	var err error
	msgs := formatter.FormatTasks(fm, nil, s.cfg.Loc)
	var digestID int64
	if formatter.TaskCount(fm.Summary) > 0 { // remember it so checkboxes can be re-rendered
		digestID, err = s.store.SaveDigest(ctx, storage.Digest{
			MeetingID: id, Title: fm.Title, Start: fm.Start, ShareLink: fm.ShareLink, Summary: fm.Summary,
		}, s.now())
		if err != nil {
			return fmt.Errorf("save digest: %w", err)
		}
	}
	// Resume after the parts already delivered so a retry never repeats them.
	for i := s.partsSent(ctx, id); i < len(msgs); i++ {
		msgID, err := s.notify.Send(ctx, msgs[i].Text, keyboard(digestID, msgs[i].Tasks, nil, formatter.DisplayTasks(fm.Summary)))
		if err != nil {
			return fmt.Errorf("send part %d/%d: %w", i+1, len(msgs), err)
		}
		if digestID != 0 {
			if err := s.store.SaveDigestMessage(ctx, digestID, i, msgID); err != nil {
				s.log.Error("save message id", "id", id, "err", err)
			}
		}
		s.setPartsSent(ctx, id, i+1)
	}
	return nil
}

// Listen handles checkbox presses until ctx is cancelled (no polling of Wispr).
func (s *Service) Listen(ctx context.Context) { s.listen(ctx) }

// markProcessed records a meeting even when ctx was just cancelled (shutdown
// right after a send), otherwise the next start would deliver it again.
func (s *Service) markProcessed(ctx context.Context, m wispr.MeetingInfo) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	_ = s.store.DeleteKV(ctx, "parts:"+m.ID)
	return s.store.MarkProcessed(ctx, m.ID, m.Title, s.now())
}

func (s *Service) release(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := s.store.Release(ctx, id); err != nil {
		s.log.Error("release claim", "id", id, "err", err)
	}
}

// tooOld reports meetings that ended longer ago than the retry/backfill window.
func (s *Service) tooOld(m wispr.MeetingInfo, now time.Time) bool {
	ref := m.End
	if ref.IsZero() {
		ref = m.Start
	}
	return !ref.IsZero() && now.Sub(ref) > max(s.cfg.Lookback, s.cfg.Backfill)
}

func (s *Service) recordFailure(m wispr.MeetingInfo, err error) {
	if errors.Is(err, errSummaryPending) {
		s.log.Debug("waiting for summary", "id", m.ID)
		return
	}
	f := s.failures[m.ID]
	if f == nil {
		f = &failure{}
		s.failures[m.ID] = f
	}
	f.n++
	backoff := s.cfg.Interval
	for i := 1; i < f.n && backoff < maxBackoff; i++ { // doubling, capped; no shift overflow
		backoff *= 2
	}
	backoff = min(backoff, maxBackoff)
	f.next = s.now().Add(backoff)
	s.log.Error("meeting failed; will retry", "id", m.ID, "attempt", f.n, "retry_in", backoff, "err", err)
}

// keyboard builds one full-width checkbox button per task shown in a message.
// display holds all tasks in display order (button n labels task n).
func keyboard(digestID int64, tasks []int, done map[int]bool, display []formatter.Task) telegram.Keyboard {
	if digestID == 0 || len(tasks) == 0 {
		return nil
	}
	var kb telegram.Keyboard
	for _, n := range tasks {
		box := "☐"
		if done[n] {
			box = "☑"
		}
		label := ""
		if n >= 1 && n <= len(display) {
			label = buttonLabel(display[n-1])
		}
		kb = append(kb, []telegram.Button{{
			Text: strings.TrimSpace(box + " " + label),
			Data: fmt.Sprintf("t:%d:%d", digestID, n),
		}})
	}
	return kb
}

// buttonLabel is "Assignee · task", cut to what fits on one line of a button.
func buttonLabel(t formatter.Task) string {
	const max = 40
	text := t.Text
	if t.Assignee != "" {
		text = t.Assignee + " · " + text
	}
	if r := []rune(text); len(r) > max {
		text = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return text
}

const offsetKey = "tg_offset"

// listen long-polls for button presses until ctx is cancelled.
func (s *Service) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.HandleUpdates(ctx, 25); err != nil && ctx.Err() == nil {
			s.log.Error("telegram updates failed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// HandleUpdates fetches pending Telegram updates (waiting up to wait seconds)
// and applies checkbox presses. The offset is stored after every update so a
// press is never applied twice.
func (s *Service) HandleUpdates(ctx context.Context, wait int) error {
	if s.cb == nil {
		return nil
	}
	var offset int64
	if v, ok, _ := s.store.GetKV(ctx, offsetKey); ok {
		offset, _ = strconv.ParseInt(v, 10, 64)
	}
	ups, err := s.cb.Updates(ctx, offset, wait)
	if err != nil {
		return err
	}
	for _, u := range ups {
		if u.CallbackQuery != nil {
			s.handleCallback(ctx, u.CallbackQuery)
		}
		if err := s.store.SetKV(ctx, offsetKey, strconv.FormatInt(u.UpdateID+1, 10)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) handleCallback(ctx context.Context, cq *telegram.CallbackQuery) {
	answer := func(text string) {
		if err := s.cb.Answer(ctx, cq.ID, text); err != nil {
			s.log.Debug("answer callback", "err", err)
		}
	}
	// Only presses in the owner's chat count; the bot is public.
	if cq.Message == nil || cq.Message.Chat.ID != s.cfg.ChatID {
		answer("")
		return
	}
	parts := strings.Split(cq.Data, ":")
	if len(parts) != 3 || parts[0] != "t" {
		answer("")
		return
	}
	digestID, err1 := strconv.ParseInt(parts[1], 10, 64)
	task, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		answer("")
		return
	}
	d, ok, err := s.store.DigestByID(ctx, digestID)
	if err != nil {
		s.log.Error("load digest", "err", err)
		answer("Ошибка, попробуйте ещё раз")
		return
	}
	if !ok {
		answer("Список устарел")
		return
	}
	if task < 1 || task > formatter.TaskCount(d.Summary) {
		answer("")
		return
	}
	nowDone, err := s.store.ToggleTask(ctx, digestID, task)
	if err != nil {
		s.log.Error("toggle task", "err", err)
		answer("Ошибка, попробуйте ещё раз")
		return
	}
	s.log.Info("task toggled", "digest", digestID, "task", task, "done", nowDone)
	if err := s.rerender(ctx, d, task, cq.Message.MessageID); err != nil {
		s.log.Error("update message", "digest", digestID, "err", err)
	}
	if nowDone {
		answer("✓ Выполнено")
	} else {
		answer("↩ Снова в работе")
	}
}

// rerender redraws the message that contains the task.
func (s *Service) rerender(ctx context.Context, d storage.Digest, task int, pressedMsg int64) error {
	done, err := s.store.DoneTasks(ctx, d.ID)
	if err != nil {
		return err
	}
	msgs := formatter.FormatTasks(formatter.Meeting{
		Title: d.Title, Start: d.Start, ShareLink: d.ShareLink, Summary: d.Summary,
	}, done, s.cfg.Loc)
	ids, err := s.store.DigestMessages(ctx, d.ID)
	if err != nil {
		return err
	}
	for i, m := range msgs {
		for _, n := range m.Tasks {
			if n != task {
				continue
			}
			id := ids[i]
			if id == 0 {
				id = pressedMsg
			}
			return s.notify.Edit(ctx, id, m.Text, keyboard(d.ID, m.Tasks, done, formatter.DisplayTasks(d.Summary)))
		}
	}
	return nil
}
