package stream

import (
	"testing"
	"time"
)

func woken(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func idle(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

func TestNotifyWakesEverySubscriberWithoutBlocking(t *testing.T) {
	s := NewSignal()
	a, _ := s.Subscribe()
	b, _ := s.Subscribe()
	s.Notify()
	s.Notify()
	if !woken(a) || !woken(b) {
		t.Fatal("a subscriber was not woken")
	}
	if !idle(a) {
		t.Fatal("two notifications left more than one pending wake-up")
	}
}

func TestCancelledSubscribersAreNotWoken(t *testing.T) {
	s := NewSignal()
	a, cancel := s.Subscribe()
	cancel()
	cancel()
	s.Notify()
	if !idle(a) {
		t.Fatal("a cancelled subscriber was woken")
	}
}

func TestCloseEndsDone(t *testing.T) {
	s := NewSignal()
	if !idle(s.Done()) {
		t.Fatal("Done closed before Close")
	}
	s.Close()
	s.Close()
	if !woken(s.Done()) {
		t.Fatal("Done did not close")
	}
}
