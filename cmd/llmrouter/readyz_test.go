package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestReadyz(t *testing.T) {
	holder := &snapshot.Holder{}
	ctx, shutdown := context.WithCancel(context.Background())
	code := func() int {
		rec := httptest.NewRecorder()
		readyz(holder, ctx)(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Code
	}
	if got := code(); got != 503 {
		t.Fatalf("before the first snapshot: %d, want 503", got)
	}
	holder.Set(&snapshot.Snapshot{})
	if got := code(); got != 200 {
		t.Fatalf("with a snapshot: %d, want 200", got)
	}
	shutdown()
	if got := code(); got != 503 {
		t.Fatalf("shutting down: %d, want 503", got)
	}
}
