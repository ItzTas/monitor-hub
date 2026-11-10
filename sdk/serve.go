package sdk

import (
	"context"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	PluginKey  = "monitor"
	DataDirEnv = "MONITOR_HUB_DATA_DIR"
)

var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "MONITOR_HUB_PLUGIN",
	MagicCookieValue: "7c0e5a52-monitor-hub",
}

type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl pluginpb.PluginServer
}

func (p *GRPCPlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	pluginpb.RegisterPluginServer(s, p.Impl)
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return pluginpb.NewPluginClient(c), nil
}

func Serve(impl pluginpb.PluginServer) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         plugin.PluginSet{PluginKey: &GRPCPlugin{Impl: impl}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

func PluginSet() plugin.PluginSet {
	return plugin.PluginSet{PluginKey: &GRPCPlugin{}}
}
