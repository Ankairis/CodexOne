package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type recordingRequestLogCleaner struct {
	cutoffs chan int64
}

func (cleaner *recordingRequestLogCleaner) DeleteOldRequestLogs(_ context.Context, before int64) (int64, error) {
	cleaner.cutoffs <- before
	return 0, nil
}

func TestRunRequestLogCleanupHandlesEveryTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 2)
	cleaner := &recordingRequestLogCleaner{cutoffs: make(chan int64, 2)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		runRequestLogCleanup(ctx, cleaner, 30, ticks, logger)
		close(done)
	}()

	times := []time.Time{
		time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC),
	}
	for _, now := range times {
		ticks <- now
	}
	for _, now := range times {
		select {
		case cutoff := <-cleaner.cutoffs:
			want := now.AddDate(0, 0, -30).UnixMilli()
			if cutoff != want {
				t.Fatalf("cleanup cutoff = %d, want %d", cutoff, want)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for scheduled cleanup")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not stop after cancellation")
	}
}
