package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	gruntime "runtime"
	"sort"
	"strings"
	"time"

	_ "github.com/containers/libpod/v2/pkg/hooks/0.1.0"
	"github.com/containers/storage/pkg/reexec"
	"github.com/cri-o/cri-o/internal/criocli"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/signals"
	"github.com/cri-o/cri-o/internal/version"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/utils"
	serverV1 "github.com/cri-o/cri-o/v1/server"
	"github.com/cri-o/cri-o/v1/server/metrics"
	serverV1alpha2 "github.com/cri-o/cri-o/v1alpha2/server"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/sirupsen/logrus"
	"github.com/soheilhy/cmux"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	runtimeV1 "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimeV1alpha2 "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
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

func catchShutdown(
	ctx context.Context,
	cancel context.CancelFunc,
	gserver *grpc.Server,
	sserverV1 *serverV1.Server,
	sserverV1alpha2 *serverV1alpha2.Server,
	hserver *http.Server,
	signalled *bool,
) {
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
			if err := sserverV1.StopStreamServer(); err != nil {
				logrus.Warnf("error shutting down streaming server: %v", err)
			}
			sserverV1.StopMonitors()
			if err := sserverV1alpha2.StopStreamServer(); err != nil {
				logrus.Warnf("error shutting down streaming server: %v", err)
			}
			sserverV1alpha2.StopMonitors()
			cancel()
			if err := sserverV1.Shutdown(ctx); err != nil {
				logrus.Warnf("error shutting down main service %v", err)
			}
			if err := sserverV1alpha2.Shutdown(ctx); err != nil {
				logrus.Warnf("error shutting down main service %v", err)
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
	// Configure klog to not write any output
	klog.LogToStderr(true)
	klog.SetOutput(ioutil.Discard)

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

		cf := &logrus.TextFormatter{
			TimestampFormat: "2006-01-02 15:04:05.000000000Z07:00",
			FullTimestamp:   true,
		}

		logrus.SetFormatter(cf)

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
				logrus.Debugf("starting profiling server on %v", profileEndpoint)
				if err := http.ListenAndServe(profileEndpoint, nil); err != nil {
					logrus.Fatalf("unable to run profiling server: %v", err)
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

		lis, err := serverV1.Listen("unix", config.Listen)
		if err != nil {
			logrus.Fatalf("failed to listen: %v", err)
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

		serviceV1, err := serverV1.New(ctx, config)
		if err != nil {
			logrus.Fatal(err)
		}

		serviceV1alpha2, err := serverV1alpha2.New(ctx, config)
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

		runtimeV1.RegisterRuntimeServiceServer(grpcServer, serviceV1)
		runtimeV1.RegisterImageServiceServer(grpcServer, serviceV1)
		runtimeV1alpha2.RegisterRuntimeServiceServer(grpcServer, serviceV1alpha2)
		runtimeV1alpha2.RegisterImageServiceServer(grpcServer, serviceV1alpha2)

		// after the daemon is done setting up we can notify systemd api
		notifySystem()

		go func() { serviceV1.StartExitMonitor() }()
		go func() { serviceV1alpha2.StartExitMonitor() }()

		hookSyncV1 := make(chan error, 2)
		if serviceV1.ContainerServer.Hooks == nil {
			hookSyncV1 <- err // so we don't block during cleanup
		} else {
			go serviceV1.ContainerServer.Hooks.Monitor(ctx, hookSyncV1)
			err = <-hookSyncV1
			if err != nil {
				cancel()
				logrus.Fatal(err)
			}
		}

		hookSyncV1alpha2 := make(chan error, 2)
		if serviceV1alpha2.ContainerServer.Hooks == nil {
			hookSyncV1alpha2 <- err // so we don't block during cleanup
		} else {
			go serviceV1alpha2.ContainerServer.Hooks.Monitor(ctx, hookSyncV1alpha2)
			err = <-hookSyncV1alpha2
			if err != nil {
				cancel()
				logrus.Fatal(err)
			}
		}

		m := cmux.New(lis)
		grpcL := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
		httpL := m.Match(cmux.HTTP1Fast())

		infoMux := serviceV1.GetInfoMux()
		httpServer := &http.Server{
			Handler:     infoMux,
			ReadTimeout: 5 * time.Second,
		}

		graceful := false
		catchShutdown(ctx, cancel, grpcServer, serviceV1, serviceV1alpha2, httpServer, &graceful)

		go func() {
			if err := grpcServer.Serve(grpcL); err != nil {
				logrus.Errorf("unable to run GRPC server: %v", err)
			}
		}()
		go func() {
			if err := httpServer.Serve(httpL); err != nil {
				logrus.Debugf("closed http server")
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

		streamServerCloseChV1 := serviceV1.StreamingServerCloseChan()
		serverMonitorsChV1 := serviceV1.MonitorsCloseChan()
		streamServerCloseChV1alpha2 := serviceV1alpha2.StreamingServerCloseChan()
		serverMonitorsChV1alpha2 := serviceV1alpha2.MonitorsCloseChan()
		select {
		case <-streamServerCloseChV1:
		case <-serverMonitorsChV1:
		case <-streamServerCloseChV1alpha2:
		case <-serverMonitorsChV1alpha2:
		case <-serverCloseCh:
		}

		if err := serviceV1.Shutdown(ctx); err != nil {
			logrus.Warnf("error shutting down v1 service: %v", err)
		}
		if err := serviceV1alpha2.Shutdown(ctx); err != nil {
			logrus.Warnf("error shutting down v1alpha2 service: %v", err)
		}
		cancel()

		<-streamServerCloseChV1
		logrus.Debug("closed v1 stream server")
		<-serverMonitorsChV1
		logrus.Debug("closed v1 monitors")

		<-streamServerCloseChV1alpha2
		logrus.Debug("closed v1alpha2 stream server")
		<-serverMonitorsChV1alpha2
		logrus.Debug("closed v1alpha2 monitors")

		err = <-hookSyncV1
		if err == nil || err == context.Canceled {
			logrus.Debug("closed v1 hook monitor")
		} else {
			logrus.Errorf("hook v1 monitor failed: %v", err)
		}

		err = <-hookSyncV1alpha2
		if err == nil || err == context.Canceled {
			logrus.Debug("closed v1alpha2 hook monitor")
		} else {
			logrus.Errorf("hook v1alpha2 monitor failed: %v", err)
		}

		<-serverCloseCh
		logrus.Debug("closed main server")

		return nil
	}

	// Log our version early at startup
	version.LogVersion()

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
