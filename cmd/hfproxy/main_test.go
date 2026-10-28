package main

import "testing"

func TestIssue83TokenScopesAreLimitedByRole(t *testing.T) {
	tests := []struct {
		role  string
		scope string
		want  bool
	}{
		{"admin", "read", true},
		{"admin", "write", true},
		{"admin", "admin", true},
		{"operator", "write", true},
		{"operator", "admin", false},
		{"reader", "read", true},
		{"reader", "write", false},
		{"admin", "unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.role+"/"+tt.scope, func(t *testing.T) {
			got := validateTokenScope(tt.role, tt.scope) == nil
			if got != tt.want {
				t.Fatalf("validateTokenScope(%q, %q) success = %v, want %v", tt.role, tt.scope, got, tt.want)
			}
		})
	}
}
