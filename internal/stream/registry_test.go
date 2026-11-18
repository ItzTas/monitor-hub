package stream

import "testing"

func TestJoiningAgainReplacesTheEarlierConnection(t *testing.T) {
	r := NewRegistry()
	first, _ := r.Join("a")
	second, _ := r.Join("a")
	_, _ = r.Join("b")
	if !woken(first) {
		t.Fatal("the earlier connection was not replaced")
	}
	if !idle(second) {
		t.Fatal("the newest connection was replaced")
	}
}

func TestLeavingUpdatesConnected(t *testing.T) {
	r := NewRegistry()
	_, leaveOld := r.Join("a")
	_, leaveNew := r.Join("a")
	if !r.Connected("a") || r.Connected("b") || r.Count() != 1 {
		t.Fatalf("Connected/Count wrong before leaving: %d", r.Count())
	}
	leaveOld()
	leaveOld()
	if !r.Connected("a") || r.Count() != 1 {
		t.Fatal("the older connection leaving removed the newer one")
	}
	leaveNew()
	leaveNew()
	if r.Connected("a") || r.Count() != 0 {
		t.Fatal("the connection is still registered after leaving")
	}
}
