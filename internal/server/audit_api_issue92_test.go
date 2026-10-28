package server

import "testing"

func TestIssue92AllowedMethodsRequireExactMatch(t *testing.T) {
	tests := []struct {
		allow  string
		method string
		want   bool
	}{
		{"GET, PATCH, DELETE", "GET", true},
		{"GET, PATCH, DELETE", "GETTING", false},
		{"POST", "OST", false},
		{"GET, POST", "DELETE", false},
	}
	for _, tt := range tests {
		t.Run(tt.allow+"/"+tt.method, func(t *testing.T) {
			if got := apiMethodAllowed(tt.allow, tt.method); got != tt.want {
				t.Fatalf("apiMethodAllowed(%q, %q) = %v, want %v", tt.allow, tt.method, got, tt.want)
			}
		})
	}
}
