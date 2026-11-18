package ingest

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
)

const HeartbeatLimit = 90 * time.Second

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
	beat    chan struct{}
	ended   chan error
	done    chan struct{}
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
			s.log.Info("the plugin has no Watch; polling it every 15s", "plugin", name)
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
			s.log.Warn("the plugin's Watch stopped; polling it every 15s", "plugin", name, "error", end.err, "retry_in", delay)
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

func (s *Service) follow(ctx context.Context, name string, client pluginpb.PluginClient, changed <-chan struct{}, sch *schedule) watchEnd {
	streamCtx, cancel := context.WithCancel(ctx)
	w := &watchStream{beat: make(chan struct{}, 1), ended: make(chan error, 1), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.ended <- s.read(streamCtx, name, client, sch, w)
	}()
	end := s.await(ctx, changed, w)
	cancel()
	<-w.done
	return settle(end, w.signals, sch)
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

func settle(end watchEnd, signals int, sch *schedule) watchEnd {
	if signals > 0 {
		sch.setWatching(false)
	}
	switch end := end.(type) {
	case dropped:
		sch.request(pollRequest{full: true})
		return dropped{established: signals >= 2, err: end.err}
	case processChanged:
		sch.request(pollRequest{full: true})
	}
	return end
}

func (s *Service) read(ctx context.Context, name string, client pluginpb.PluginClient, sch *schedule, w *watchStream) error {
	stream, err := client.Watch(ctx, &pluginpb.WatchRequest{})
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

func (s *Service) signal(name string, sch *schedule, w *watchStream, sig *pluginpb.WatchResponse) {
	w.signals++
	if w.signals > 1 {
		sch.request(pollRequest{latestSeq: sig.GetLatestSeq()})
		return
	}
	sch.setWatching(true)
	sch.request(pollRequest{full: true})
	s.log.Info("watching a plugin", "plugin", name)
}
