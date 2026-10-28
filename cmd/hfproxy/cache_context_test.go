package main

import (
	"testing"
	"time"
)

func TestIssue74CacheOperationContextHasNoGlobalDeadline(t *testing.T) {
	ctx, cancel := newCacheOperationContext()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("cache operation context has a global deadline")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cache operation context did not cancel")
	}
}
