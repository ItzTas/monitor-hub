package monitorconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/ItzTas/monitor-hub/src/internal/supervisor"
)

type statusClient struct {
	monitorpb.MonitorClient
	answer   *monitorpb.StatusResponse
	err      error
	deadline time.Duration
}

func (c *statusClient) Status(ctx context.Context, _ *monitorpb.StatusRequest, _ ...grpc.CallOption) (*monitorpb.StatusResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.deadline = time.Until(deadline)
	}
	return c.answer, c.err
}

func TestProgress(t *testing.T) {
	s := newSaveScene(t, integerFields, nil)
	s.supervisor.setAttempt(supervisor.Attempt{Phase: supervisor.PhaseRunning, Settled: 0})
	if got, err := s.service.Progress(t.Context(), "testmonitor"); err != nil || !reflect.DeepEqual(got, Progress{State: ProgressNone, Phase: supervisor.PhaseRunning}) {
		t.Errorf("Progress before any save = %+v, %v; want none", got, err)
	}
	s.save(t, Change{Secrets: map[string]*string{"p": text("row-db-secret-value")}})
	requestedAt := startedAt
	idle := &statusClient{answer: &monitorpb.StatusResponse{State: monitorpb.State_STATE_IDLE, Message: "ok"}}
	broken := &statusClient{err: errors.New("connection refused")}

	cases := []struct {
		name    string
		attempt supervisor.Attempt
		client  *statusClient
		later   time.Duration
		want    Progress
	}{
		{"starting", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseStarting}, nil, 0,
			Progress{State: ProgressRestarting, Phase: supervisor.PhaseStarting}},
		{"running and answering", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseRunning, Settled: 1}, idle, 0,
			Progress{State: ProgressUp, Phase: supervisor.PhaseRunning, MonitorState: monitorpb.State_STATE_IDLE, Message: "ok"}},
		{"running without an answer", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseRunning, Settled: 1}, broken, 0,
			Progress{State: ProgressRestarting, Phase: supervisor.PhaseRunning}},
		{"running without an answer for 75 s", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseRunning, Settled: 1}, broken, 75 * time.Second,
			Progress{State: ProgressNoAnswer, Phase: supervisor.PhaseRunning}},
		{"running without a client", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseRunning, Settled: 1}, nil, 74 * time.Second,
			Progress{State: ProgressRestarting, Phase: supervisor.PhaseRunning}},
		{"failed", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseFailed, Settled: 1, Reason: "connecting to row-db-secret-value: refused"}, nil, 0,
			Progress{State: ProgressFailed, Phase: supervisor.PhaseFailed, Reason: "connecting to [redacted]: refused"}},
		{"starting again after a failure", supervisor.Attempt{Request: 1, Phase: supervisor.PhaseStarting, Settled: 1, Reason: "refused"}, nil, 0,
			Progress{State: ProgressFailed, Phase: supervisor.PhaseStarting, Reason: "refused"}},
		{"a failure before the request", supervisor.Attempt{Phase: supervisor.PhaseFailed, Reason: "old"}, nil, 0,
			Progress{State: ProgressRestarting, Phase: supervisor.PhaseFailed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s.supervisor.setAttempt(c.attempt)
			s.supervisor.setClient(nil)
			if c.client != nil {
				s.supervisor.setClient(c.client)
			}
			s.clock.set(startedAt.Add(c.later))
			want := c.want
			want.Request, want.RequestedAt = 1, &requestedAt
			got, err := s.service.Progress(t.Context(), "testmonitor")
			if err != nil {
				t.Fatalf("Progress: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Progress = %+v, want %+v", got, want)
			}
		})
	}
	if idle.deadline <= 4*time.Second || idle.deadline > 5*time.Second {
		t.Errorf("Status had %v to answer, want 5 s", idle.deadline)
	}
}

func TestProgressCutsTheReasonTo500RunesAfterTakingOutTheSecrets(t *testing.T) {
	s := newSaveScene(t, integerFields, nil)
	secret := saveSecrets["stored secret"]
	s.save(t, Change{Secrets: map[string]*string{"p": text(secret)}})
	before := strings.Repeat("é", 495) + " "
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseExited, Settled: 1, Reason: before + secret + ": refused"})
	got, err := s.service.Progress(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if strings.Contains(got.Reason, secret[:4]) {
		t.Fatal("the reason holds the start of the stored secret")
	}
	if want := before + "[red"; got.Reason != want {
		t.Errorf("reason = %q (%d runes), want %q", got.Reason, utf8.RuneCountInString(got.Reason), want)
	}
}

func TestProgressOfAnUnknownMonitorAndADownDatabase(t *testing.T) {
	store := &flakyStore{Store: newStore(t)}
	s := newSaveSceneOn(t, store, integerFields, nil)
	if _, err := s.service.Progress(t.Context(), "nope"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Progress of an unknown monitor = %v, want ErrUnknown", err)
	}
	s.save(t, Change{Document: raws(t, `{"n":1}`)})
	store.fail()
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseFailed, Settled: 1, Reason: "refused"})
	if _, err := s.service.Progress(t.Context(), "testmonitor"); !errors.Is(err, ErrDatabase) {
		t.Errorf("Progress with the database down = %v, want ErrDatabase", err)
	}
}

func TestLastStartHidesSecrets(t *testing.T) {
	s := newSaveScene(t, integerFields, nil)
	secret := saveSecrets["stored secret"]
	s.save(t, Change{Secrets: map[string]*string{"p": text(secret)}})
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseFailed, Settled: 1, Reason: "connecting to " + secret + ": refused"})
	got, err := s.service.LastStart(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("LastStart: %v", err)
	}
	if strings.Contains(got.Reason, secret) {
		t.Fatal("the reason holds the stored secret")
	}
	if want := (StartReport{Phase: supervisor.PhaseFailed, Reason: "connecting to [redacted]: refused"}); got != want {
		t.Errorf("LastStart = %+v, want %+v", got, want)
	}
}

func TestLastStartCutsTheReasonTo500RunesAfterTakingOutTheSecrets(t *testing.T) {
	s := newSaveScene(t, integerFields, nil)
	secret := saveSecrets["stored secret"]
	s.save(t, Change{Secrets: map[string]*string{"p": text(secret)}})
	before := strings.Repeat("é", 495) + " "
	s.supervisor.setAttempt(supervisor.Attempt{Request: 1, Phase: supervisor.PhaseExited, Settled: 1, Reason: before + secret + ": refused"})
	got, err := s.service.LastStart(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("LastStart: %v", err)
	}
	if strings.Contains(got.Reason, secret[:4]) {
		t.Fatal("the reason holds the start of the stored secret")
	}
	if want := before + "[red"; got.Reason != want {
		t.Errorf("reason = %q (%d runes), want %q", got.Reason, utf8.RuneCountInString(got.Reason), want)
	}
}

func TestLastStartWithoutAReasonReadsNothing(t *testing.T) {
	down := &flakyStore{down: true}
	s := newSaveSceneOn(t, down, integerFields, nil)
	s.supervisor.setAttempt(supervisor.Attempt{Phase: supervisor.PhaseNotStarted})
	got, err := s.service.LastStart(t.Context(), "testmonitor")
	if err != nil {
		t.Fatalf("LastStart: %v", err)
	}
	if want := (StartReport{Phase: supervisor.PhaseNotStarted}); got != want {
		t.Errorf("LastStart = %+v, want %+v", got, want)
	}
	if down.gets != 0 {
		t.Errorf("LastStart without a reason read the database %d times", down.gets)
	}
}

func TestLastStartOfAnUndeclaredMonitorReadsNothing(t *testing.T) {
	down := &flakyStore{down: true}
	s := newSaveSceneOn(t, down, integerFields, nil)
	first := strings.Repeat("é", 500)
	s.supervisor.setAttempt(supervisor.Attempt{Phase: supervisor.PhaseExited, Reason: first + strings.Repeat("x", 100)})
	got, err := s.service.LastStart(t.Context(), "bare")
	if err != nil {
		t.Fatalf("LastStart: %v", err)
	}
	if want := (StartReport{Phase: supervisor.PhaseExited, Reason: first}); got != want {
		t.Errorf("LastStart = %+v (%d runes), want the first 500 runes", got, utf8.RuneCountInString(got.Reason))
	}
	if down.gets != 0 {
		t.Errorf("LastStart of a monitor without a declaration read the database %d times", down.gets)
	}
}

func TestLastStartErrors(t *testing.T) {
	store := &flakyStore{Store: newStore(t)}
	s := newSaveSceneOn(t, store, integerFields, nil)
	if _, err := s.service.LastStart(t.Context(), "nope"); !errors.Is(err, ErrUnknown) {
		t.Errorf("LastStart of an unknown monitor = %v, want ErrUnknown", err)
	}
	store.fail()
	s.supervisor.setAttempt(supervisor.Attempt{Phase: supervisor.PhaseFailed, Reason: "refused"})
	if _, err := s.service.LastStart(t.Context(), "testmonitor"); !errors.Is(err, ErrDatabase) {
		t.Errorf("LastStart with the database down = %v, want ErrDatabase", err)
	}
}
