package server

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestIssue86LoginRateLimitMapIsBoundedAndPrunesExpiredBuckets(t *testing.T) {
	s := &Server{loginBuckets: make(map[string]loginBucket, loginRateLimitMaxBuckets)}
	now := time.Now()
	for i := 0; i < loginRateLimitMaxBuckets; i++ {
		s.loginBuckets["user:"+strconv.Itoa(i)] = loginBucket{window: now, count: 1}
	}
	req := httptest.NewRequest("POST", "/login", nil)
	if _, allowed := s.allowLoginAttempt(req, "new-user"); allowed {
		t.Fatal("login attempt was allowed beyond the bucket map capacity")
	}
	if len(s.loginBuckets) != loginRateLimitMaxBuckets {
		t.Fatalf("bucket count = %d, want cap %d", len(s.loginBuckets), loginRateLimitMaxBuckets)
	}
	for key, bucket := range s.loginBuckets {
		bucket.window = now.Add(-2 * time.Minute)
		s.loginBuckets[key] = bucket
	}
	if _, allowed := s.allowLoginAttempt(req, "new-user"); !allowed {
		t.Fatal("attempt should be allowed after expired buckets are pruned")
	}
	if len(s.loginBuckets) != 2 {
		t.Fatalf("bucket count after pruning = %d, want 2", len(s.loginBuckets))
	}
}
