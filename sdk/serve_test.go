package sdk

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ItzTas/monitor-hub/sdk/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

func TestAPluginWithoutWatchAnswersUnimplemented(t *testing.T) {
	server := grpc.NewServer()
	if err := (&GRPCPlugin{Impl: stubServer{}}).GRPCServer(nil, server); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stream, err := pluginpb.NewPluginClient(conn).Watch(ctx, &pluginpb.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Recv code = %v (%v), want Unimplemented", status.Code(err), err)
	}
}
