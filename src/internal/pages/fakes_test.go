package pages

import (
	"context"
	"sync"

	"github.com/ItzTas/monitor-hub/src/internal/access"
	"github.com/ItzTas/monitor-hub/src/internal/accounts"
)

// fakeAccounts answers like accounts.Service: an unknown cookie is
// ErrUnauthenticated, and any pair but login/password is wrongPassword.
type fakeAccounts struct {
	mu            sync.Mutex
	sessions      map[string]access.Principal
	sessionErr    error
	login         string
	password      string
	session       accounts.Session
	principal     access.Principal
	wrongPassword error
	loggedOut     []string
	calls         map[string]int
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		sessions:      map[string]access.Principal{},
		wrongPassword: accounts.ErrWrongPassword,
		calls:         map[string]int{},
	}
}

func (f *fakeAccounts) BySession(_ context.Context, id string) (access.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["BySession"]++
	if f.sessionErr != nil {
		return access.Principal{}, f.sessionErr
	}
	p, ok := f.sessions[id]
	if !ok {
		return access.Principal{}, accounts.ErrUnauthenticated
	}
	return p, nil
}

func (f *fakeAccounts) Login(_ context.Context, login, password string) (accounts.Session, access.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Login"]++
	if login != f.login || password != f.password {
		return accounts.Session{}, access.Principal{}, f.wrongPassword
	}
	return f.session, f.principal, nil
}

func (f *fakeAccounts) Logout(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Logout"]++
	f.loggedOut = append(f.loggedOut, sessionID)
	return nil
}

func (f *fakeAccounts) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}
