package server

import (
	"io"
	"testing"
)

func TestIssue107DoNotRestartAfterStreamingResumedBytes(t *testing.T) {
	if !resumeRestartWouldCorruptResponse(io.Discard, 1024) {
		t.Fatal("restart after a streamed prefix must abort the response")
	}
	if resumeRestartWouldCorruptResponse(io.Discard, 0) {
		t.Fatal("restart before streaming any bytes is safe")
	}
	if resumeRestartWouldCorruptResponse(nil, 1024) {
		t.Fatal("non-streamed preload can restart safely")
	}
}
