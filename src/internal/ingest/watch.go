package ingest

// The hub keeps one Watch call open per monitor and follows the monitor's current process. A signal asks for a poll; while the call is down, the schedule polls every 15 seconds.

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
)

const (
	HeartbeatLimit   = 90 * time.Second
	EstablishedAfter = 30 * time.Second
)

var reopenDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute}

type watchEnd interface {
	watchEnd()
}

type stopped struct{}

type processChanged struct{}

type unimplemented struct{}

type dropped struct {
	established bool
	err         error
}

func (stopped) watchEnd()        {}
func (processChanged) watchEnd() {}
func (unimplemented) watchEnd()  {}
func (dropped) watchEnd()        {}

type watchStream struct {
	beat  chan struct{}
	ended chan error
	done  chan struct{}
	// Written only by the goroutine that reads the stream; follow reads it after done is closed.
	signals int
}

func (s *Service) subscribe(ctx context.Context, name string, sch *schedule) {
	failures := 0
	for {
		client, ok, changed := s.clients.Track(name)
		if !ok {
			failures = 0
			if !waitChange(ctx, changed) {
				return
			}
			continue
		}
		switch end := s.follow(ctx, name, client, changed, sch).(type) {
		case stopped:
			return
		case processChanged:
			failures = 0
		case unimplemented:
			s.log.Info("the monitor has no Watch; polling it every 15s", "monitor", name)
			sch.request(pollRequest{full: true})
			if !waitChange(ctx, changed) {
				return
			}
			failures = 0
		case dropped:
			if end.established {
				failures = 0
			}
			delay := reopenDelays[min(failures, len(reopenDelays)-1)]
			failures++
			s.log.Warn("the monitor's Watch stopped; polling it every 15s", "monitor", name, "error", end.err, "retry_in", delay)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				failures = 0
			case <-s.after(delay):
			}
		}
	}
}

func waitChange(ctx context.Context, changed <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return true
	}
}

func (s *Service) follow(ctx context.Context, name string, client monitorpb.MonitorClient, changed <-chan struct{}, sch *schedule) watchEnd {
	opened := s.now()
	streamCtx, cancel := context.WithCancel(ctx)
	w := &watchStream{beat: make(chan struct{}, 1), ended: make(chan error, 1), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.ended <- s.read(streamCtx, name, client, sch, w)
	}()
	end := s.await(ctx, changed, w)
	lived := s.now().Sub(opened)
	cancel()
	<-w.done
	established := w.signals >= 2 && lived >= EstablishedAfter
	backToPolling(end, w.signals > 0, sch)
	if end, ok := end.(dropped); ok {
		end.established = established
		return end
	}
	return end
}

func (s *Service) await(ctx context.Context, changed <-chan struct{}, w *watchStream) watchEnd {
	silence := s.after(HeartbeatLimit)
	for {
		select {
		case <-ctx.Done():
			return stopped{}
		case <-changed:
			return processChanged{}
		case <-w.beat:
			silence = s.after(HeartbeatLimit)
		case <-silence:
			return dropped{err: fmt.Errorf("no signal for %s", HeartbeatLimit)}
		case err := <-w.ended:
			return ended(ctx, changed, err, w.signals > 0)
		}
	}
}

func ended(ctx context.Context, changed <-chan struct{}, err error, signalled bool) watchEnd {
	if ctx.Err() != nil {
		return stopped{}
	}
	select {
	case <-changed:
		return processChanged{}
	default:
	}
	if !signalled && status.Code(err) == codes.Unimplemented {
		return unimplemented{}
	}
	return dropped{err: err}
}

func backToPolling(end watchEnd, signalled bool, sch *schedule) {
	if signalled {
		sch.setWatching(false)
	}
	switch end.(type) {
	case dropped, processChanged:
		sch.request(pollRequest{full: true})
	}
}

func (s *Service) read(ctx context.Context, name string, client monitorpb.MonitorClient, sch *schedule, w *watchStream) error {
	stream, err := client.Watch(ctx, &monitorpb.WatchRequest{})
	if err != nil {
		return err
	}
	for {
		sig, err := stream.Recv()
		if err != nil {
			return err
		}
		s.signal(name, sch, w, sig)
		select {
		case w.beat <- struct{}{}:
		default:
		}
	}
}

func (s *Service) signal(name string, sch *schedule, w *watchStream, sig *monitorpb.WatchResponse) {
	w.signals++
	if w.signals > 1 {
		sch.request(pollRequest{latestSeq: sig.GetLatestSeq()})
		return
	}
	sch.setWatching(true)
	sch.request(pollRequest{full: true})
	s.log.Info("watching a monitor", "monitor", name)
}
