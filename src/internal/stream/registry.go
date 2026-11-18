package stream

import "sync"

type Registry struct {
	mu    sync.Mutex
	conns map[string]chan struct{}
}

func NewRegistry() *Registry {
	return &Registry{conns: map[string]chan struct{}{}}
}

func (r *Registry) Join(name string) (<-chan struct{}, func()) {
	replaced := make(chan struct{})
	r.mu.Lock()
	if old, ok := r.conns[name]; ok {
		close(old)
	}
	r.conns[name] = replaced
	r.mu.Unlock()
	return replaced, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.conns[name] == replaced {
			delete(r.conns, name)
		}
	}
}

func (r *Registry) Connected(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.conns[name]
	return ok
}

func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
