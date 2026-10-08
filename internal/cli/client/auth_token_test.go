package client

import (
	"context"
	"net"
	"testing"

	pb "github.com/gelotto/hqsshd/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Remote connections never sent the daemon's auth_token, so a daemon with
// auth_token set refused every hqssh -H command. The tunnel's dial options
// must carry it in the "authorization: Bearer <token>" form validateAuth
// (and the mobile app) use.
func TestRemoteDialOptionsSendBearerToken(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"s3cret", "Bearer s3cret"},
		{"", ""},
	} {
		got := make(chan string, 1)
		srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			v := ""
			if vals := md.Get("authorization"); len(vals) > 0 {
				v = vals[0]
			}
			got <- v
			return &pb.SystemInfo{}, nil
		}))
		pb.RegisterSystemServiceServer(srv, pb.UnimplementedSystemServiceServer{})
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(lis)

		conn, err := grpc.NewClient(lis.Addr().String(), remoteDialOptions(tc.token)...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pb.NewSystemServiceClient(conn).GetInfo(context.Background(), &pb.Empty{}); err != nil {
			t.Fatalf("GetInfo: %v", err)
		}
		if v := <-got; v != tc.want {
			t.Errorf("token %q: authorization = %q, want %q", tc.token, v, tc.want)
		}
		conn.Close()
		srv.Stop()
	}
}
