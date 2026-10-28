package server

import (
	"encoding/base64"
	"reflect"
	"testing"
)

func TestIssue89ListCursorSupportsPipeInFields(t *testing.T) {
	want := []string{"ml|research", "group|id"}
	got, ok := decodeListCursor(encodeListCursor(want...), len(want))
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor round trip = %#v, %v; want %#v", got, ok, want)
	}
}

func TestIssue89ListCursorAcceptsPreviousFormat(t *testing.T) {
	previous := base64.RawURLEncoding.EncodeToString([]byte("team|id"))
	got, ok := decodeListCursor(previous, 2)
	if !ok || !reflect.DeepEqual(got, []string{"team", "id"}) {
		t.Fatalf("legacy cursor decode = %#v, %v", got, ok)
	}
}
