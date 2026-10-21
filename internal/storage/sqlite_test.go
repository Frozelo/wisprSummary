package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if done, _ := s.IsProcessed(ctx, "a"); done {
		t.Fatal("unexpected processed")
	}
	if _, ok, _ := s.Watermark(ctx); ok {
		t.Fatal("unexpected watermark")
	}
	wm := time.Date(2026, 10, 21, 1, 2, 3, 456, time.UTC)
	s.MarkProcessed(ctx, "a", "A", wm)
	s.MarkProcessed(ctx, "a", "A again", wm) // idempotent
	s.SetWatermark(ctx, wm)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if done, _ := s.IsProcessed(ctx, "a"); !done {
		t.Fatal("processed id lost after reopen")
	}
	if got, ok, _ := s.Watermark(ctx); !ok || !got.Equal(wm) {
		t.Fatalf("watermark = %v, %v", got, ok)
	}
}

func TestClaimIsExclusiveUntilReleasedOrExpired(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ttl := 10 * time.Minute

	if ok, _ := s.Claim(ctx, "a", now, ttl); !ok {
		t.Fatal("first claim must succeed")
	}
	if ok, _ := s.Claim(ctx, "a", now.Add(time.Minute), ttl); ok {
		t.Fatal("second claim inside the ttl must fail")
	}
	if ok, _ := s.Claim(ctx, "a", now.Add(11*time.Minute), ttl); !ok {
		t.Fatal("an expired claim must be taken over")
	}
	s.Release(ctx, "a")
	if ok, _ := s.Claim(ctx, "a", now.Add(12*time.Minute), ttl); !ok {
		t.Fatal("claim after release must succeed")
	}
	s.MarkProcessed(ctx, "a", "A", now) // also clears the claim
	if ok, _ := s.Claim(ctx, "a", now.Add(13*time.Minute), ttl); !ok {
		t.Fatal("MarkProcessed should clear the claim")
	}
}

func TestKV(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, _ := s.GetKV(ctx, "k"); ok {
		t.Fatal("unexpected value")
	}
	s.SetKV(ctx, "k", "1")
	s.SetKV(ctx, "k", "2")
	if v, ok, _ := s.GetKV(ctx, "k"); !ok || v != "2" {
		t.Fatalf("v=%q ok=%v", v, ok)
	}
	s.DeleteKV(ctx, "k")
	if _, ok, _ := s.GetKV(ctx, "k"); ok {
		t.Fatal("not deleted")
	}
}

func TestDigestsAndTaskMarks(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	d := Digest{MeetingID: "m1", Title: "T", Start: now, ShareLink: "https://x", Summary: "### Следующие шаги\n- a\n"}

	id, err := s.SaveDigest(ctx, d, now)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.SaveDigest(ctx, d, now); again != id {
		t.Fatalf("SaveDigest is not idempotent: %d vs %d", again, id)
	}
	got, ok, _ := s.DigestByID(ctx, id)
	if !ok || got.Title != "T" || !got.Start.Equal(now) || got.Summary != d.Summary {
		t.Fatalf("digest = %+v ok=%v", got, ok)
	}
	if _, ok, _ := s.DigestByID(ctx, 999); ok {
		t.Fatal("unknown digest found")
	}

	s.SaveDigestMessage(ctx, id, 0, 111)
	s.SaveDigestMessage(ctx, id, 1, 222)
	s.SaveDigestMessage(ctx, id, 1, 333) // resend replaces
	msgs, _ := s.DigestMessages(ctx, id)
	if len(msgs) != 2 || msgs[0] != 111 || msgs[1] != 333 {
		t.Fatalf("messages = %v", msgs)
	}

	if done, _ := s.ToggleTask(ctx, id, 2); !done {
		t.Fatal("first toggle should mark done")
	}
	if m, _ := s.DoneTasks(ctx, id); !m[2] || len(m) != 1 {
		t.Fatalf("done = %v", m)
	}
	if done, _ := s.ToggleTask(ctx, id, 2); done {
		t.Fatal("second toggle should clear")
	}
	if m, _ := s.DoneTasks(ctx, id); len(m) != 0 {
		t.Fatalf("done = %v", m)
	}

	s.ToggleTask(ctx, id, 1)
	if err := s.PruneDigests(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.DigestByID(ctx, id); ok {
		t.Fatal("digest not pruned")
	}
	if m, _ := s.DigestMessages(ctx, id); len(m) != 0 {
		t.Fatal("messages not pruned")
	}
	if m, _ := s.DoneTasks(ctx, id); len(m) != 0 {
		t.Fatal("marks not pruned")
	}
}
