package sdk

import (
	"testing"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"google.golang.org/grpc"
)

type stubServer struct {
	monitorpb.UnimplementedMonitorServer
}

func TestMonitorSetHoldsTheMonitor(t *testing.T) {
	set := MonitorSet()
	if _, ok := set[MonitorKey].(*GRPCMonitor); !ok {
		t.Fatalf("MonitorSet()[%q] = %T, want *GRPCMonitor", MonitorKey, set[MonitorKey])
	}
}

func TestGRPCServerRegistersImpl(t *testing.T) {
	s := grpc.NewServer()
	p := &GRPCMonitor{Impl: stubServer{}}
	if err := p.GRPCServer(nil, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetServiceInfo()["monitorhub.monitor.v1.Monitor"]; !ok {
		t.Fatal("service monitorhub.monitor.v1.Monitor not registered")
	}
}

func TestHandshakeValues(t *testing.T) {
	if Handshake.ProtocolVersion != 2 || Handshake.MagicCookieKey != "MONITOR_HUB_MONITOR" || Handshake.MagicCookieValue != "7c0e5a52-monitor-hub" {
		t.Fatalf("unexpected handshake %+v", Handshake)
	}
}
