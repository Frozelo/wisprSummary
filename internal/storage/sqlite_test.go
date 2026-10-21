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
