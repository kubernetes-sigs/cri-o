package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/server/cri/experimental"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"golang.org/x/net/context"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

var checkpointCommand = cli.Command{
	Name:                   "checkpoint",
	Usage:                  "Checkpoints one or more containers/pods",
	ArgsUsage:              "CONTAINER-ID [POD-ID...]",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "export",
			Aliases: []string{"e"},
			Usage:   "Specify the name of the tar archive used to export the checkpoint image.",
		},
		&cli.BoolFlag{
			Name:    "keep",
			Aliases: []string{"k"},
			Usage:   "Keep all temporary checkpoint files.",
		},
		&cli.BoolFlag{
			Name:    "leave-running",
			Aliases: []string{"R"},
			Usage:   "Leave the container running after writing checkpoint to disk.",
		},
		&cli.BoolFlag{
			Name:  "tcp-established",
			Usage: "Checkpoint a container with established TCP connections.",
		},
		&cli.StringFlag{
			Name:    "compress",
			Aliases: []string{"c"},
			Usage:   "Select compression algorithm (gzip, none, zstd) for checkpoint archive.",
			Value:   "zstd",
		},
	},

	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			return cli.ShowSubcommandHelp(c)
		}
		address := c.String("connect")
		timeout := c.Duration("timeout")
		conn, err := getClientConnection(address, timeout)
		if err != nil {
			return fmt.Errorf("failed to connect: %v", err)
		}
		defer conn.Close()
		client := experimental.NewRuntimeServiceClient(conn)

		var compression archive.Compression
		switch strings.ToLower(c.String("compress")) {
		case "none":
			compression = archive.Uncompressed
		case "gzip":
			compression = archive.Gzip
		case "zstd":
			compression = archive.Zstd
		default:
			return errors.Errorf(
				"Selected compression algorithm (%q) not supported\n",
				c.String("compress"),
			)
		}

		request := &experimental.CheckpointContainerRequest{
			Options: &experimental.CheckpointContainerOptions{
				CommonOptions: &experimental.CheckpointRestoreOptions{
					Archive:        c.String("export"),
					Keep:           c.Bool("keep"),
					TcpEstablished: c.Bool("tcp-established"),
					Compression:    int64(compression),
				},
				LeaveRunning: c.Bool("leave-running"),
			},
		}
		for i := 0; i < c.NArg(); i++ {
			request.Id = c.Args().Get(i)
			logrus.Debugf("CheckpointContainerRequest: %#v", request)
			r, err := client.CheckpointContainer(context.Background(), request)
			logrus.Debugf("CheckpointContainerResponse: %#v", r)
			if err != nil {
				return err
			}
			fmt.Println(request.Id)
		}
		return nil
	},
}

var restoreCommand = cli.Command{
	Name:                   "restore",
	Usage:                  "Restore one or more containers/pods",
	ArgsUsage:              "CONTAINER-ID [POD-ID...]",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "import",
			Aliases: []string{"i"},
			Usage:   "Restore from exported checkpoint/pod archive.",
		},
		&cli.BoolFlag{
			Name:    "keep",
			Aliases: []string{"k"},
			Usage:   "Keep all temporary checkpoint and restore files.",
		},
		&cli.StringFlag{
			Name:    "pod",
			Aliases: []string{"p"},
			Usage:   "Specify POD into which the container will be restored. Defaults to previous POD.",
		},
		&cli.BoolFlag{
			Name:  "tcp-established",
			Usage: "Restore a container with established TCP connections.",
		},
	},

	Action: func(c *cli.Context) error {
		if c.NArg() == 0 && c.String("import") == "" {
			return cli.ShowSubcommandHelp(c)
		}
		address := c.String("connect")
		timeout := c.Duration("timeout")
		conn, err := getClientConnection(address, timeout)
		if err != nil {
			return fmt.Errorf("failed to connect: %v", err)
		}
		defer conn.Close()
		client := experimental.NewRuntimeServiceClient(conn)

		request := &experimental.RestoreContainerRequest{
			Options: &experimental.RestoreContainerOptions{
				PodSandboxId: func() string {
					if c.IsSet("pod") {
						return c.String("pod")
					}
					return ""
				}(),
				CommonOptions: &experimental.CheckpointRestoreOptions{
					Archive:        c.String("import"),
					Keep:           c.Bool("keep"),
					TcpEstablished: c.Bool("tcp-established"),
				},
			},
		}

		var ids []string

		if c.NArg() > 0 {
			for i := 0; i < c.NArg(); i++ {
				ids = append(ids, c.Args().Get(i))
			}
		} else {
			ids = append(ids, "")
		}

		var rs []*experimental.RestoreContainerResponse
		for _, i := range ids {
			request.Id = i
			logrus.Debugf("RestoreContainerRequest: %#v", request)
			r, err := client.RestoreContainer(context.Background(), request)
			logrus.Debugf("RestoreContainerResponse: %#v", r)
			if err != nil {
				return err
			}
			rs = append(rs, r)
		}

		result, err := json.MarshalIndent(rs, "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", result)

		return nil
	},
}

func getClientConnection(address string, timeout time.Duration) (*grpc.ClientConn, error) {
	dialer := func(_ context.Context, _ string) (net.Conn, error) {
		return net.DialTimeout("unix", address, timeout)
	}
	conn, err := grpc.DialContext(
		context.Background(),
		address,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %v", err)
	}
	return conn, nil
}

func main() {
	app := cli.NewApp()
	app.Name = "crio-cr"
	app.Usage = "trigger container/pod checkpoints or restores"

	app.Commands = []*cli.Command{
		&checkpointCommand,
		&restoreCommand,
	}

	sort.Sort(cli.FlagsByName(checkpointCommand.Flags))

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:  "connect",
			Usage: "Socket to connect to",
			Value: "/run/crio/crio.sock",
		},
		&cli.BoolFlag{
			Aliases: []string{"d"},
			Name:    "debug",
			Usage:   "Enable debug output",
		},
		&cli.DurationFlag{
			Name:  "timeout",
			Usage: "Timeout of connecting to server",
			Value: 10 * time.Second,
		},
	}

	app.Before = func(c *cli.Context) error {
		if c.Bool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		return nil
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
