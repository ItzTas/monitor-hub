package sdk

import (
	"context"

	"github.com/ItzTas/monitor-hub/sdk-go/monitorpb"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	PluginKey  = "monitor"
	DataDirEnv = "MONITOR_HUB_DATA_DIR"
)

var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  2,
	MagicCookieKey:   "MONITOR_HUB_MONITOR",
	MagicCookieValue: "7c0e5a52-monitor-hub",
}

type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl monitorpb.MonitorServer
}

func (p *GRPCPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	monitorpb.RegisterMonitorServer(s, p.Impl)
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return monitorpb.NewMonitorClient(c), nil
}

func Serve(impl monitorpb.MonitorServer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         plugin.PluginSet{PluginKey: &GRPCPlugin{Impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

func PluginSet() plugin.PluginSet {
	return plugin.PluginSet{PluginKey: &GRPCPlugin{}}
}
