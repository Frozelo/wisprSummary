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
	"strings"
	"time"

	"github.com/Frozelo/wisprSummary/internal/formatter"
	"github.com/Frozelo/wisprSummary/internal/wispr"
)

const (
	maxPages   = 20 // Wispr caps paging at 1000 results per query
	claimTTL   = 10 * time.Minute
	maxBackoff = 30 * time.Minute
	dbTimeout  = 5 * time.Second
)

// Source is the Wispr side (implemented by *wispr.Client).
type Source interface {
	SearchMeetings(ctx context.Context, cursor string, limit int) (wispr.MeetingPage, error)
	GetMeeting(ctx context.Context, id string) (wispr.Meeting, error)
}

// Notifier delivers one HTML message to the owner's chat.
type Notifier interface {
	Send(ctx context.Context, html string) error
}

type Store interface {
	IsProcessed(ctx context.Context, id string) (bool, error)
	MarkProcessed(ctx context.Context, id, title string, at time.Time) error
	Claim(ctx context.Context, id string, now time.Time, ttl time.Duration) (bool, error)
	Release(ctx context.Context, id string) error
	Watermark(ctx context.Context) (time.Time, bool, error)
	SetWatermark(ctx context.Context, t time.Time) error
}

type Config struct {
	Interval     time.Duration // poll period
	Lookback     time.Duration // longest a pending/failed meeting is retried
	Backfill     time.Duration // on first run, also send meetings modified this recently
	Slack        time.Duration // overlap kept when advancing the watermark
	SummaryGrace time.Duration // wait this long after a meeting ends for its summary
	PageSize     int
	Loc          *time.Location
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
}

func NewService(cfg Config, src Source, n Notifier, st Store, log *slog.Logger) *Service {
	cfg.setDefaults()
	return &Service{cfg: cfg, src: src, notify: n, store: st, log: log, now: time.Now, failures: map[string]*failure{}, sent: map[string]int{}}
}

// Run polls until ctx is cancelled. Errors never stop the loop; a lost Wispr
// login triggers a single Telegram alert until polling works again.
func (s *Service) Run(ctx context.Context) error {
	alerted := false
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		err := s.Tick(ctx)
		switch {
		case err == nil:
			alerted = false
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, wispr.ErrLoginRequired):
			s.log.Error("wispr login required; run `bot login`")
			if !alerted {
				alerted = s.notify.Send(ctx, "⚠️ Wispr login expired. Run <code>bot login</code> to resume summaries.") == nil
			}
		default:
			s.log.Error("poll failed", "err", err)
		}
		t.Reset(s.cfg.Interval)
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
	msgs := formatter.Format(formatter.Meeting{
		Title: m.Title, Start: m.Start, ShareLink: m.ShareLink, Summary: m.Summary,
	}, s.cfg.Loc)
	// Resume after the parts already delivered so a retry never repeats them.
	for i := s.sent[info.ID]; i < len(msgs); i++ {
		if err := s.notify.Send(ctx, msgs[i]); err != nil {
			return fmt.Errorf("send part %d/%d: %w", i+1, len(msgs), err)
		}
		s.sent[info.ID] = i + 1
	}
	if err := s.markProcessed(ctx, info); err != nil {
		return fmt.Errorf("mark processed (message was sent): %w", err)
	}
	s.log.Info("meeting sent", "id", info.ID)
	return nil
}

// markProcessed records a meeting even when ctx was just cancelled (shutdown
// right after a send), otherwise the next start would deliver it again.
func (s *Service) markProcessed(ctx context.Context, m wispr.MeetingInfo) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
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
