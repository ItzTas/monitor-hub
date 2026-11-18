package ingest

import (
	"sync"
	"time"
)

const (
	SafetyInterval = 5 * time.Minute
	followUpDelay  = 250 * time.Millisecond
)

type pollRequest struct {
	full      bool
	latestSeq int64
}

func (r pollRequest) merge(o pollRequest) pollRequest {
	return pollRequest{full: r.full || o.full, latestSeq: max(r.latestSeq, o.latestSeq)}
}

func (r pollRequest) readsEvents(cursor int64, known bool) bool {
	return r.full || !known || r.latestSeq > cursor
}

type schedule struct {
	mu       sync.Mutex
	next     pollRequest
	queued   bool
	watching bool
	kick     chan struct{}
}

func newSchedule() *schedule {
	return &schedule{kick: make(chan struct{}, 1)}
}

func (s *schedule) request(r pollRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = s.next.merge(r)
	s.queued = true
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *schedule) take() (pollRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.kick:
	default:
	}
	r, ok := s.next, s.queued
	s.next, s.queued = pollRequest{}, false
	return r, ok
}

func (s *schedule) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued
}

func (s *schedule) setWatching(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watching = on
}

func (s *schedule) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watching {
		return SafetyInterval
	}
	return PollInterval
}
