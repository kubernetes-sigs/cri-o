package main

import (
    "github.com/cri-o/cri-o/internal/log"
    "golang.org/x/net/context"
    pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

var shutdownCommand = &cli.Command{
	Name:  "shutdown",
	Usage: "Shutdown CRI-O containers before shutting down the system",
	Action: func(c *cli.Context) error {
        // FIXME: how to pass in the `s *Server` and `ctx context.Context` here?
        s.stopAllPodSandboxes(ctx)
	},
}
