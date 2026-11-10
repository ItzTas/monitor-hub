package sdk

import (
	"testing"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"google.golang.org/grpc"
)

type stubServer struct {
	pluginpb.UnimplementedPluginServer
}

func TestPluginSetHoldsTheMonitorPlugin(t *testing.T) {
	set := PluginSet()
	if _, ok := set[PluginKey].(*GRPCPlugin); !ok {
		t.Fatalf("PluginSet()[%q] = %T, want *GRPCPlugin", PluginKey, set[PluginKey])
	}
}

func TestGRPCServerRegistersImpl(t *testing.T) {
	s := grpc.NewServer()
	p := &GRPCPlugin{Impl: stubServer{}}
	if err := p.GRPCServer(nil, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetServiceInfo()["monitorhub.plugin.v1.Plugin"]; !ok {
		t.Fatal("service monitorhub.plugin.v1.Plugin not registered")
	}
}

func TestHandshakeValues(t *testing.T) {
	if Handshake.ProtocolVersion != 1 || Handshake.MagicCookieKey != "MONITOR_HUB_PLUGIN" || Handshake.MagicCookieValue != "7c0e5a52-monitor-hub" {
		t.Fatalf("unexpected handshake %+v", Handshake)
	}
}
