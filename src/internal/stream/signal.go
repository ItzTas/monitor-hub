package stream

import "sync"

type Signal struct {
	mu   sync.Mutex
	subs map[*chan struct{}]struct{}
	done chan struct{}
	once sync.Once
}

func NewSignal() *Signal {
	return &Signal{subs: map[*chan struct{}]struct{}{}, done: make(chan struct{})}
}

func (s *Signal) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case *sub <- struct{}{}:
		default:
		}
	}
}

func (s *Signal) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[&ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, &ch)
		s.mu.Unlock()
	}
}

func (s *Signal) Close() {
	s.once.Do(func() { close(s.done) })
}

func (s *Signal) Done() <-chan struct{} {
	return s.done
}
