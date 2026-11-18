package sdk

import (
	"context"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	MonitorKey = "monitor"
	DataDirEnv = "MONITOR_HUB_DATA_DIR"
)

var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  2,
	MagicCookieKey:   "MONITOR_HUB_MONITOR",
	MagicCookieValue: "7c0e5a52-monitor-hub",
}

type GRPCMonitor struct {
	plugin.NetRPCUnsupportedPlugin
	Impl monitorpb.MonitorServer
}

func (p *GRPCMonitor) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	monitorpb.RegisterMonitorServer(s, p.Impl)
	return nil
}

func (p *GRPCMonitor) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return monitorpb.NewMonitorClient(c), nil
}

func Serve(impl monitorpb.MonitorServer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         plugin.PluginSet{MonitorKey: &GRPCMonitor{Impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

func MonitorSet() plugin.PluginSet {
	return plugin.PluginSet{MonitorKey: &GRPCMonitor{}}
}
