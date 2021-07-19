package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containers/image/v5/types"
	storageTypes "github.com/containers/storage"
	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/resourcestore"
	"github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/internal/version"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
)

const (
	shutdownFile        = "/var/lib/crio/crio.shutdown"
	certRefreshInterval = time.Minute * 5
	rootlessEnvName     = "_CRIO_ROOTLESS"
)

// StreamService implements streaming.Runtime.
type StreamService struct {
	ctx                 context.Context
	runtimeServer       *Server // needed by Exec() endpoint
	streamServer        streaming.Server
	streamServerCloseCh chan struct{}
	streaming.Runtime
}

// Server implements the RuntimeService and ImageService
type Server struct {
	config          libconfig.Config
	stream          StreamService
	hostportManager hostport.HostPortManager

	*lib.ContainerServer
	monitorsChan      chan struct{}
	defaultIDMappings *idtools.IDMappings

	updateLock sync.RWMutex

	// pullOperationsInProgress is used to avoid pulling the same image in parallel. Goroutines
	// will block on the pullResult.
	pullOperationsInProgress map[pullArguments]*pullOperation
	// pullOperationsLock is used to synchronize pull operations.
	pullOperationsLock sync.Mutex

	resourceStore *resourcestore.ResourceStore
}

// pullArguments are used to identify a pullOperation via an input image name and
// possibly specified credentials.
type pullArguments struct {
	image       string
	credentials types.DockerAuthConfig
}

// pullOperation is used to synchronize parallel pull operations via the
// server's pullCache.  Goroutines can block the pullOperation's waitgroup and
// be released once the pull operation has finished.
type pullOperation struct {
	// wg allows for Goroutines trying to pull the same image to wait until the
	// currently running pull operation has finished.
	wg sync.WaitGroup
	// imageRef is the reference of the actually pulled image which will differ
	// from the input if it was a short name (e.g., alpine).
	imageRef string
	// err is the error indicating if the pull operation has succeeded or not.
	err error
}

type certConfigCache struct {
	config  *tls.Config
	expires time.Time

	tlsCert string
	tlsKey  string
	tlsCA   string
}

// GetConfigForClient gets the tlsConfig for the streaming server.
// This allows the certs to be swapped, without shutting down crio.
func (cc *certConfigCache) GetConfigForClient(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if cc.config != nil && time.Now().Before(cc.expires) {
		return cc.config, nil
	}
	config := new(tls.Config)
	cert, err := tls.LoadX509KeyPair(cc.tlsCert, cc.tlsKey)
	if err != nil {
		return nil, err
	}
	config.Certificates = []tls.Certificate{cert}
	if len(cc.tlsCA) > 0 {
		caBytes, err := ioutil.ReadFile(cc.tlsCA)
		if err != nil {
			return nil, err
		}
		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(caBytes)
		config.ClientCAs = certPool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	cc.config = config
	cc.expires = time.Now().Add(certRefreshInterval)
	return config, nil
}

// StopStreamServer stops the stream server
func (s *Server) StopStreamServer() error {
	return s.stream.streamServer.Stop()
}

// StreamingServerCloseChan returns the close channel for the streaming server
func (s *Server) StreamingServerCloseChan() chan struct{} {
	return s.stream.streamServerCloseCh
}

// getExec returns exec stream request
func (s *Server) getExec(req *pb.ExecRequest) (*pb.ExecResponse, error) {
	return s.stream.streamServer.GetExec(req)
}

// getAttach returns attach stream request
func (s *Server) getAttach(req *pb.AttachRequest) (*pb.AttachResponse, error) {
	return s.stream.streamServer.GetAttach(req)
}

// getPortForward returns port forward stream request
func (s *Server) getPortForward(req *pb.PortForwardRequest) (*pb.PortForwardResponse, error) {
	return s.stream.streamServer.GetPortForward(req)
}

// restore attempts to restore the sandboxes and containers.
// For every sandbox it fails to restore, it starts a cleanup routine attempting to call CNI DEL
// For every container it fails to restore, it returns that containers image, so that
// it can be cleaned up (if we're using internal_wipe).
func (s *Server) restore(ctx context.Context) []string {
	containersAndTheirImages := map[string]string{}
	containers, err := s.Store().Containers()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logrus.Warnf("could not read containers and sandboxes: %v", err)
	}
	pods := map[string]*storage.RuntimeContainerMetadata{}
	podContainers := map[string]*storage.RuntimeContainerMetadata{}
	names := map[string][]string{}
	deletedPods := map[string]*sandbox.Sandbox{}
	for i := range containers {
		metadata, err2 := s.StorageRuntimeServer().GetContainerMetadata(containers[i].ID)
		if err2 != nil {
			logrus.Warnf("error parsing metadata for %s: %v, ignoring", containers[i].ID, err2)
			continue
		}
		if !storage.IsCrioContainer(&metadata) {
			logrus.Debugf("container %s determined to not be a CRI-O container or sandbox", containers[i].ID)
			continue
		}
		names[containers[i].ID] = containers[i].Names
		if metadata.Pod {
			pods[containers[i].ID] = &metadata
		} else {
			podContainers[containers[i].ID] = &metadata
			containersAndTheirImages[containers[i].ID] = containers[i].ImageID
		}
	}

	// Go through all the pods and check if it can be restored. If an error occurs, delete the pod and any containers
	// associated with it. Release the pod and container names as well.
	for sbID := range pods {
		sb, err := s.LoadSandbox(sbID)
		if err == nil {
			continue
		}
		logrus.Warnf("could not restore sandbox %s; deleting it and containers underneath it: %v", sbID, err)
		for _, n := range names[sbID] {
			if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
				logrus.Warnf("unable to delete container %s: %v", n, err)
			}
			// Release the infra container name and the pod name for future use
			if strings.Contains(n, infraName) {
				s.ReleaseContainerName(n)
			} else {
				s.ReleasePodName(n)
			}
		}
		for k, v := range podContainers {
			if v.PodID != sbID {
				continue
			}
			for _, n := range names[k] {
				if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
					logrus.Warnf("unable to delete container %s: %v", n, err)
				}
				// Release the container name for future use
				s.ReleaseContainerName(n)
			}
			// Remove the container from the list of podContainers, or else we'll retry the delete later,
			// causing a useless debug message.
			delete(podContainers, k)
		}
		// Add the pod id to the list of deletedPods, to be able to call CNI DEL on the sandbox network.
		// Unfortunately, if we weren't able to restore a sandbox, then there's little that can be done
		if sb != nil {
			deletedPods[sbID] = sb
		}
	}

	// Go through all the containers and check if it can be restored. If an error occurs, delete the conainer and
	// release the name associated with you.
	for containerID := range podContainers {
		err := s.LoadContainer(containerID)
		if err == nil || err == lib.ErrIsNonCrioContainer {
			delete(containersAndTheirImages, containerID)
			continue
		}
		logrus.Warnf("Could not restore container %s: %v", containerID, err)
		for _, n := range names[containerID] {
			if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
				logrus.Warnf("Unable to delete container %s: %v", n, err)
			}
			// Release the container name
			s.ReleaseContainerName(n)
		}
	}

	// Cleanup the deletedPods in the networking plugin
	wipeResourceCleaner := resourcestore.NewResourceCleaner()
	for _, sb := range deletedPods {
		sb := sb
		cleanupFunc := func() error {
			err := s.networkStop(context.Background(), sb)
			if err == nil {
				logrus.Infof("Successfully cleaned up network for pod %s", sb.ID())
			}
			return err
		}
		wipeResourceCleaner.Add(ctx, "cleanup sandbox network", cleanupFunc)
	}

	// If any failed to be deleted, the networking plugin is likely not ready.
	// The cleanup should be retried until it succeeds.
	go func() {
		if err := wipeResourceCleaner.Cleanup(); err != nil {
			logrus.Errorf("Cleanup during server startup failed: %v", err)
		}
	}()

	// Restore sandbox IPs
	for _, sb := range s.ListSandboxes() {
		ips, err := s.getSandboxIPs(sb)
		if err != nil {
			logrus.Warnf("Could not restore sandbox IP for %v: %v", sb.ID(), err)
			continue
		}
		sb.AddIPs(ips)
	}

	// Return a slice of images to remove, if internal_wipe is set.
	imagesOfDeletedContainers := []string{}
	for _, image := range containersAndTheirImages {
		imagesOfDeletedContainers = append(imagesOfDeletedContainers, image)
	}

	return imagesOfDeletedContainers
}

// cleanupSandboxesOnShutdown Remove all running Sandboxes on system shutdown
func (s *Server) cleanupSandboxesOnShutdown(ctx context.Context) {
	_, err := os.Stat(shutdownFile)
	if err == nil || !os.IsNotExist(err) {
		logrus.Debugf("shutting down all sandboxes, on shutdown")
		s.stopAllPodSandboxes(ctx)
		err = os.Remove(shutdownFile)
		if err != nil {
			logrus.Warnf("Failed to remove %q", shutdownFile)
		}
	}
}

// Shutdown attempts to shut down the server's storage cleanly
func (s *Server) Shutdown(ctx context.Context) error {
	// why do this on clean shutdown! we want containers left running when crio
	// is down for whatever reason no?!
	// notice this won't trigger just on system halt but also on normal
	// crio.service restart!!!
	s.cleanupSandboxesOnShutdown(ctx)
	s.resourceStore.Close()

	return s.ContainerServer.Shutdown()
}

// configureMaxThreads sets the Go runtime max threads threshold
// which is 90% of the kernel setting from /proc/sys/kernel/threads-max
func configureMaxThreads() error {
	mt, err := ioutil.ReadFile("/proc/sys/kernel/threads-max")
	if err != nil {
		return err
	}
	mtint, err := strconv.Atoi(strings.TrimSpace(string(mt)))
	if err != nil {
		return err
	}
	maxThreads := (mtint / 100) * 90
	debug.SetMaxThreads(maxThreads)
	logrus.Debugf("Golang's threads limit set to %d", maxThreads)
	return nil
}

func getIDMappings(config *libconfig.Config) (*idtools.IDMappings, error) {
	if config.UIDMappings == "" || config.GIDMappings == "" {
		return nil, nil
	}

	parsedUIDsMappings, err := idtools.ParseIDMap(strings.Split(config.UIDMappings, ","), "UID")
	if err != nil {
		return nil, err
	}
	parsedGIDsMappings, err := idtools.ParseIDMap(strings.Split(config.GIDMappings, ","), "GID")
	if err != nil {
		return nil, err
	}

	return idtools.NewIDMappingsFromMaps(parsedUIDsMappings, parsedGIDsMappings), nil
}

// New creates a new Server with the provided context and configuration
func New(
	ctx context.Context,
	configIface libconfig.Iface,
) (*Server, error) {
	if configIface == nil || configIface.GetData() == nil {
		return nil, fmt.Errorf("provided configuration interface or its data is nil")
	}
	config := configIface.GetData()

	config.SystemContext.AuthFilePath = config.GlobalAuthFile
	config.SystemContext.SignaturePolicyPath = config.SignaturePolicyPath

	if err := os.MkdirAll(config.ContainerAttachSocketDir, 0o755); err != nil {
		return nil, err
	}

	// This is used to monitor container exits using inotify
	if err := os.MkdirAll(config.ContainerExitsDir, 0o755); err != nil {
		return nil, err
	}
	containerServer, err := lib.New(ctx, configIface)
	if err != nil {
		return nil, err
	}

	hostportManager := hostport.NewMetaHostportManager()

	idMappings, err := getIDMappings(config)
	if err != nil {
		return nil, err
	}

	if os.Getenv(rootlessEnvName) == "" {
		// Not running as rootless, reset XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS
		os.Unsetenv("XDG_RUNTIME_DIR")
		os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	}

	s := &Server{
		ContainerServer:          containerServer,
		hostportManager:          hostportManager,
		config:                   *config,
		monitorsChan:             make(chan struct{}),
		defaultIDMappings:        idMappings,
		pullOperationsInProgress: make(map[pullArguments]*pullOperation),
		resourceStore:            resourcestore.New(),
	}

	if err := configureMaxThreads(); err != nil {
		return nil, err
	}

	deletedImages := s.restore(ctx)
	s.cleanupSandboxesOnShutdown(ctx)
	s.wipeIfAppropriate(ctx, deletedImages)

	var bindAddressStr string
	bindAddress := net.ParseIP(config.StreamAddress)
	if bindAddress != nil {
		bindAddressStr = bindAddress.String()
	}

	_, err = net.LookupPort("tcp", config.StreamPort)
	if err != nil {
		return nil, err
	}

	// Prepare streaming server
	streamServerConfig := streaming.DefaultConfig
	if config.StreamIdleTimeout != "" {
		idleTimeout, err := time.ParseDuration(config.StreamIdleTimeout)
		if err != nil {
			return nil, errors.New("unable to parse timeout as duration")
		}

		streamServerConfig.StreamIdleTimeout = idleTimeout
	}
	streamServerConfig.Addr = net.JoinHostPort(bindAddressStr, config.StreamPort)
	if config.StreamEnableTLS {
		certCache := &certConfigCache{
			tlsCert: config.StreamTLSCert,
			tlsKey:  config.StreamTLSKey,
			tlsCA:   config.StreamTLSCA,
		}
		// We add the certs to the config, even thought the config is dynamic, because
		// the http package method, ServeTLS, checks to make sure there is a cert in the
		// config or it throws an error.
		cert, err := tls.LoadX509KeyPair(config.StreamTLSCert, config.StreamTLSKey)
		if err != nil {
			return nil, err
		}
		streamServerConfig.TLSConfig = &tls.Config{
			GetConfigForClient: certCache.GetConfigForClient,
			Certificates:       []tls.Certificate{cert},
		}
	}
	s.stream.ctx = ctx
	s.stream.runtimeServer = s
	s.stream.streamServer, err = streaming.NewServer(streamServerConfig, s.stream)
	if err != nil {
		return nil, fmt.Errorf("unable to create streaming server")
	}

	s.stream.streamServerCloseCh = make(chan struct{})
	go func() {
		defer close(s.stream.streamServerCloseCh)
		if err := s.stream.streamServer.Start(true); err != nil && err != http.ErrServerClosed {
			logrus.Fatalf("Failed to start streaming server: %v", err)
		}
	}()

	logrus.Debugf("sandboxes: %v", s.ContainerServer.ListSandboxes())

	// Start a configuration watcher for the default config
	s.config.StartWatcher()

	// Start the metrics server if configured to be enabled
	if err := s.startMetricsServer(); err != nil {
		return nil, err
	}

	return s, nil
}

// wipeIfAppropriate takes a list of images. If the config's VersionFilePersist
// indicates an upgrade has happened, it attempts to wipe that list of images.
// This attempt is best-effort.
func (s *Server) wipeIfAppropriate(ctx context.Context, imagesToDelete []string) {
	if !s.config.InternalWipe {
		return
	}
	// Check if our persistent version file is out of date.
	// If so, we have upgrade, and we should wipe images.
	shouldWipeImages, err := version.ShouldCrioWipe(s.config.VersionFilePersist)
	if err != nil {
		logrus.Warnf("error encountered when checking whether cri-o should wipe images: %v", err)
	}
	// Note: some of these will fail if some aspect of the pod cleanup failed as well,
	// but this is best-effort anyway, as the Kubelet will eventually cleanup images when
	// disk usage gets too high.
	if shouldWipeImages {
		for _, img := range imagesToDelete {
			if err := s.removeImage(ctx, img); err != nil {
				logrus.Warnf("failed to remove image %s: %v", img, err)
			}
		}
	}
}

func (s *Server) addSandbox(sb *sandbox.Sandbox) error {
	return s.ContainerServer.AddSandbox(sb)
}

func (s *Server) getSandbox(id string) *sandbox.Sandbox {
	return s.ContainerServer.GetSandbox(id)
}

func (s *Server) removeSandbox(id string) error {
	return s.ContainerServer.RemoveSandbox(id)
}

func (s *Server) addContainer(c *oci.Container) {
	s.ContainerServer.AddContainer(c)
}

func (s *Server) addInfraContainer(c *oci.Container) {
	s.ContainerServer.AddInfraContainer(c)
}

func (s *Server) getInfraContainer(id string) *oci.Container {
	return s.ContainerServer.GetInfraContainer(id)
}

func (s *Server) removeContainer(c *oci.Container) {
	s.ContainerServer.RemoveContainer(c)
}

func (s *Server) removeInfraContainer(c *oci.Container) {
	s.ContainerServer.RemoveInfraContainer(c)
}

func (s *Server) getPodSandboxFromRequest(podSandboxID string) (*sandbox.Sandbox, error) {
	if podSandboxID == "" {
		return nil, sandbox.ErrIDEmpty
	}

	sandboxID, err := s.PodIDIndex().Get(podSandboxID)
	if err != nil {
		return nil, fmt.Errorf("PodSandbox with ID starting with %s not found: %v", podSandboxID, err)
	}

	sb := s.getSandbox(sandboxID)
	if sb == nil {
		return nil, fmt.Errorf("specified pod sandbox not found: %s", sandboxID)
	}
	return sb, nil
}

func (s *Server) startMetricsServer() error {
	if !s.config.EnableMetrics {
		return nil
	}

	me, err := s.CreateMetricsEndpoint()
	if err != nil {
		return errors.Wrap(err, "failed to create metrics endpoint")
	}

	if err := startMetricsEndpoint(
		"tcp", fmt.Sprintf(":%v", s.config.MetricsPort), me,
	); err != nil {
		return errors.Wrap(err, "creating tcp metrics endpoint")
	}

	metricsSocket := s.config.MetricsSocket
	if metricsSocket != "" {
		if err := libconfig.RemoveUnusedSocket(metricsSocket); err != nil {
			return errors.Wrapf(err, "removing ununsed socket %s", metricsSocket)
		}

		return errors.Wrap(
			startMetricsEndpoint("unix", s.config.MetricsSocket, me),
			"creating path metrics endpoint",
		)
	}

	return nil
}

func startMetricsEndpoint(network, address string, me *http.ServeMux) error {
	l, err := net.Listen(network, address)
	if err != nil {
		return errors.Wrap(err, "creating listener")
	}

	go func() {
		logrus.Infof("Serving metrics on %s", address)
		if err := http.Serve(l, me); err != nil {
			logrus.Fatalf("failed to serve metrics endpoint %v: %v", l, err)
		}
	}()

	return nil
}

// CreateMetricsEndpoint creates a /metrics endpoint
// for prometheus monitoring
func (s *Server) CreateMetricsEndpoint() (*http.ServeMux, error) {
	metrics.Register()
	mux := &http.ServeMux{}
	mux.Handle("/metrics", promhttp.Handler())
	return mux, nil
}

// StopMonitors stops all the monitors
func (s *Server) StopMonitors() {
	close(s.monitorsChan)
}

// MonitorsCloseChan returns the close chan for the exit monitor
func (s *Server) MonitorsCloseChan() chan struct{} {
	return s.monitorsChan
}

// StartExitMonitor start a routine that monitors container exits
// and updates the container status
func (s *Server) StartExitMonitor() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Fatalf("Failed to create new watch: %v", err)
	}
	defer watcher.Close()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				logrus.Debugf("event: %v", event)
				if event.Op&fsnotify.Create == fsnotify.Create {
					containerID := filepath.Base(event.Name)
					logrus.Debugf("container or sandbox exited: %v", containerID)
					c := s.GetContainer(containerID)
					if c != nil {
						logrus.Debugf("container exited and found: %v", containerID)
						err := s.Runtime().UpdateContainerStatus(c)
						if err != nil {
							logrus.Warnf("Failed to update container status %s: %v", containerID, err)
						} else if err := s.ContainerStateToDisk(c); err != nil {
							logrus.Warnf("unable to write containers %s state to disk: %v", c.ID(), err)
						}
					} else {
						sb := s.GetSandbox(containerID)
						if sb != nil {
							c := sb.InfraContainer()
							if c == nil {
								logrus.Warnf("no infra container set for sandbox: %v", containerID)
								continue
							}
							logrus.Debugf("sandbox exited and found: %v", containerID)
							err := s.Runtime().UpdateContainerStatus(c)
							if err != nil {
								logrus.Warnf("Failed to update sandbox infra container status %s: %v", c.ID(), err)
							} else if err := s.ContainerStateToDisk(c); err != nil {
								logrus.Warnf("unable to write containers %s state to disk: %v", c.ID(), err)
							}
						}
					}
				}
			case err := <-watcher.Errors:
				logrus.Debugf("watch error: %v", err)
				close(done)
				return
			case <-s.monitorsChan:
				logrus.Debug("closing exit monitor...")
				close(done)
				return
			}
		}
	}()
	if err := watcher.Add(s.config.ContainerExitsDir); err != nil {
		logrus.Errorf("watcher.Add(%q) failed: %s", s.config.ContainerExitsDir, err)
		close(done)
	}
	<-done
}
