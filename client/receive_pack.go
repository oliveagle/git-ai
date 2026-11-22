package client

import (
	"context"
	"io"

	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/gitaly/gitalyclient"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ReceivePack proxies an SSH git-receive-pack (git push) session to Gitaly
func ReceivePack(ctx context.Context, conn *grpc.ClientConn, stdin io.Reader, stdout, stderr io.Writer, req *gitalypb.SSHReceivePackRequest) (int32, error) {
	return gitalyclient.ReceivePack(ctx, conn, stdin, stdout, stderr, req)
}
