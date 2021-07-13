package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	gruntime "runtime"
	"sort"
	"strings"
	"time"

	_ "github.com/containers/podman/v3/pkg/hooks/0.1.0"
	"github.com/containers/storage/pkg/reexec"
	"github.com/cri-o/cri-o/internal/criocli"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/signals"
	"github.com/cri-o/cri-o/internal/version"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server"
	"github.com/cri-o/cri-o/server/cri/experimental"
	v1 "github.com/cri-o/cri-o/server/cri/v1"
	"github.com/cri-o/cri-o/server/cri/v1alpha2"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/utils"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

func writeCrioGoroutineStacks() {
	path := filepath.Join("/tmp", fmt.Sprintf(
		"crio-goroutine-stacks-%s.log",
		strings.ReplaceAll(time.Now().Format(time.RFC3339), ":", ""),
	))
	if err := utils.WriteGoroutineStacksToFile(path); err != nil {
		logrus.Warnf("Failed to write goroutine stacks: %s", err)
	}
}

func catchShutdown(ctx context.Context, cancel context.CancelFunc, gserver *grpc.Server, sserver *server.Server, hserver *http.Server, signalled *bool) {
	sig := make(chan os.Signal, 2048)
	signal.Notify(sig, signals.Interrupt, signals.Term, unix.SIGUSR1, unix.SIGUSR2, unix.SIGPIPE, signals.Hup)
	go func() {
		for s := range sig {
			logrus.WithFields(logrus.Fields{
				"signal": s,
			}).Debug("received signal")
			switch s {
			case unix.SIGUSR1:
				writeCrioGoroutineStacks()
				continue
			case unix.SIGUSR2:
				gruntime.GC()
				continue
			case unix.SIGPIPE:
				continue
			case signals.Interrupt:
				logrus.Debugf("Caught SIGINT")
			case signals.Term:
				logrus.Debugf("Caught SIGTERM")
			default:
				continue
			}
			*signalled = true
			gserver.GracefulStop()
			hserver.Shutdown(ctx) // nolint: errcheck
			if err := sserver.StopStreamServer(); err != nil {
				logrus.Warnf("Error shutting down streaming server: %v", err)
			}
			sserver.StopMonitors()
			cancel()
			if err := sserver.Shutdown(ctx); err != nil {
				logrus.Warnf("Error shutting down main service %v", err)
			}
			return
		}
	}()
}

const usage = `OCI-based implementation of Kubernetes Container Runtime Interface Daemon

crio is meant to provide an integration path between OCI conformant runtimes
and the kubelet. Specifically, it implements the Kubelet Container Runtime
Interface (CRI) using OCI conformant runtimes. The scope of crio is tied to the
scope of the CRI.

1. Support multiple image formats including the existing Docker and OCI image formats.
2. Support for multiple means to download images including trust & image verification.
3. Container image management (managing image layers, overlay filesystems, etc).
4. Container process lifecycle management.
5. Monitoring and logging required to satisfy the CRI.
6. Resource isolation as required by the CRI.`

func main() {
	log.InitKlogShim()

	if reexec.Init() {
		fmt.Fprintf(os.Stderr, "unable to initialize container storage\n")
		os.Exit(-1)
	}
	app := cli.NewApp()

	app.Name = "crio"
	app.Usage = "OCI-based implementation of Kubernetes Container Runtime Interface"
	app.Authors = []*cli.Author{{Name: "The CRI-O Maintainers"}}
	app.UsageText = usage
	app.Description = app.Usage
	app.Version = version.Version + "\n" + version.Get().String()

	var err error
	app.Flags, app.Metadata, err = criocli.GetFlagsAndMetadata()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	sort.Sort(cli.FlagsByName(app.Flags))
	sort.Sort(cli.FlagsByName(configCommand.Flags))

	app.Commands = criocli.DefaultCommands
	app.Commands = append(app.Commands, []*cli.Command{
		configCommand,
		versionCommand,
		wipeCommand,
	}...)

	app.Before = func(c *cli.Context) (err error) {
		config, err := criocli.GetAndMergeConfigFromContext(c)
		if err != nil {
			return err
		}

		logrus.SetFormatter(&logrus.TextFormatter{
			TimestampFormat: "2006-01-02 15:04:05.000000000Z07:00",
			FullTimestamp:   true,
		})
		version.LogVersion()

		level, err := logrus.ParseLevel(config.LogLevel)
		if err != nil {
			return err
		}
		logrus.SetLevel(level)
		logrus.AddHook(log.NewFilenameHook())

		filterHook, err := log.NewFilterHook(config.LogFilter)
		if err != nil {
			return err
		}
		logrus.AddHook(filterHook)

		if path := c.String("log"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_SYNC, 0o666)
			if err != nil {
				return err
			}
			logrus.SetOutput(f)
		}

		switch c.String("log-format") {
		case "text":
			// retain logrus's default.
		case "json":
			logrus.SetFormatter(new(logrus.JSONFormatter))
		default:
			return fmt.Errorf("unknown log-format %q", c.String("log-format"))
		}

		return nil
	}

	app.Action = func(c *cli.Context) error {
		ctx, cancel := context.WithCancel(context.Background())
		if c.Bool("profile") {
			profilePort := c.Int("profile-port")
			profileEndpoint := fmt.Sprintf("localhost:%v", profilePort)
			go func() {
				logrus.Debugf("Starting profiling server on %v", profileEndpoint)
				if err := http.ListenAndServe(profileEndpoint, nil); err != nil {
					logrus.Fatalf("Unable to run profiling server: %v", err)
				}
			}()
		}

		if c.Args().Len() > 0 {
			cancel()
			return fmt.Errorf("command %q not supported", c.Args().Get(0))
		}

		config, ok := c.App.Metadata["config"].(*libconfig.Config)
		if !ok {
			cancel()
			return fmt.Errorf("type assertion error when accessing server config")
		}

		// Validate the configuration during runtime
		if err := config.Validate(true); err != nil {
			cancel()
			return err
		}

		lis, err := server.Listen("unix", config.Listen)
		if err != nil {
			logrus.Fatalf("Failed to listen: %v", err)
		}

		if err := os.Chmod(config.Listen, 0o660); err != nil {
			logrus.Fatalf("Failed to chmod listen socket %s: %v", config.Listen, err)
		}

		grpcServer := grpc.NewServer(
			grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
				metrics.UnaryInterceptor(),
				log.UnaryInterceptor(),
			)),
			grpc.StreamInterceptor(log.StreamInterceptor()),
			grpc.MaxSendMsgSize(config.GRPCMaxSendMsgSize),
			grpc.MaxRecvMsgSize(config.GRPCMaxRecvMsgSize),
		)

		crioServer, err := server.New(ctx, config)
		if err != nil {
			logrus.Fatal(err)
		}

		// Immediately upon start up, write our new version files
		// we write one to a tmpfs, so we can detect when a node rebooted.
		// in this sitaution, we want to wipe containers
		if err := version.WriteVersionFile(config.VersionFile); err != nil {
			logrus.Fatal(err)
		}
		// we then write to a persistent directory. This is to check if crio has upgraded
		// if it has, we should wipe images
		if err := version.WriteVersionFile(config.VersionFilePersist); err != nil {
			logrus.Fatal(err)
		}

		if config.CleanShutdownFile != "" {
			// clear out the shutdown file
			if err := os.Remove(config.CleanShutdownFile); err != nil {
				// not a fatal error, as it could have been cleaned up
				logrus.Error(err)
			}

			// Write "$CleanShutdownFile".supported to show crio-wipe that
			// we should be wiping if the CleanShutdownFile wasn't found.
			// This protects us from wiping after an upgrade from a version that don't support
			// CleanShutdownFile.
			f, err := os.Create(config.CleanShutdownSupportedFileName())
			if err != nil {
				logrus.Errorf("Writing clean shutdown supported file: %v", err)
			}
			f.Close()

			// and sync the changes to disk
			if err := utils.SyncParent(config.CleanShutdownFile); err != nil {
				logrus.Errorf("Failed to sync parent directory of clean shutdown file: %v", err)
			}
		}

		experimental.Register(grpcServer, crioServer)
		v1alpha2.Register(grpcServer, crioServer)
		v1.Register(grpcServer, crioServer)

		// after the daemon is done setting up we can notify systemd api
		notifySystem()

		go func() {
			crioServer.StartExitMonitor(ctx)
		}()
		hookSync := make(chan error, 2)
		if crioServer.ContainerServer.Hooks == nil {
			hookSync <- err // so we don't block during cleanup
		} else {
			go crioServer.ContainerServer.Hooks.Monitor(ctx, hookSync)
			err = <-hookSync
			if err != nil {
				cancel()
				logrus.Fatal(err)
			}
		}

		m := cmux.New(lis)
		grpcL := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
		httpL := m.Match(cmux.HTTP1Fast())

		infoMux := crioServer.GetInfoMux(c.Bool("enable-profile-unix-socket"))
		httpServer := &http.Server{
			Handler:     infoMux,
			ReadTimeout: 5 * time.Second,
		}

		graceful := false
		catchShutdown(ctx, cancel, grpcServer, crioServer, httpServer, &graceful)

		go func() {
			if err := grpcServer.Serve(grpcL); err != nil {
				logrus.Errorf("Unable to run GRPC server: %v", err)
			}
		}()
		go func() {
			if err := httpServer.Serve(httpL); err != nil {
				logrus.Debugf("Closed http server")
			}
		}()

		serverCloseCh := make(chan struct{})
		go func() {
			defer close(serverCloseCh)
			if err := m.Serve(); err != nil {
				if graceful && strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
					err = nil
				} else {
					logrus.Errorf("Failed to serve grpc request: %v", err)
				}
			}
		}()

		streamServerCloseCh := crioServer.StreamingServerCloseChan()
		serverMonitorsCh := crioServer.MonitorsCloseChan()
		select {
		case <-streamServerCloseCh:
		case <-serverMonitorsCh:
		case <-serverCloseCh:
		}

		if err := crioServer.Shutdown(ctx); err != nil {
			logrus.Warnf("Error shutting down service: %v", err)
		}
		cancel()

		<-streamServerCloseCh
		logrus.Debugf("Closed stream server")
		<-serverMonitorsCh
		logrus.Debugf("Closed monitors")
		err = <-hookSync
		if err == nil || err == context.Canceled {
			logrus.Debugf("Closed hook monitor")
		} else {
			logrus.Errorf("Hook monitor failed: %v", err)
		}
		<-serverCloseCh
		logrus.Debugf("Closed main server")

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
