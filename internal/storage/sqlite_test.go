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
