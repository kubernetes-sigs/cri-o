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

	imageTypes "github.com/containers/image/v5/types"
	"github.com/containers/storage/pkg/idtools"
	storageTypes "github.com/containers/storage/types"
	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/resourcestore"
	"github.com/cri-o/cri-o/internal/runtimehandlerhooks"
	"github.com/cri-o/cri-o/internal/storage"
	"github.com/cri-o/cri-o/internal/version"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/server/streaming"
	"github.com/cri-o/cri-o/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	shutdownFile        = "/var/lib/crio/crio.shutdown"
	certRefreshInterval = time.Minute * 5
	rootlessEnvName     = "_CRIO_ROOTLESS"
)

var errSandboxNotCreated = errors.New("sandbox not created")

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
	tracerName      string

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
	image         string
	sandboxCgroup string
	credentials   imageTypes.DockerAuthConfig
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
			return nil, errors.Wrap(err, "read TLS CA file")
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
func (s *Server) getExec(req *types.ExecRequest) (*types.ExecResponse, error) {
	return s.stream.streamServer.GetExec(req)
}

// getAttach returns attach stream request
func (s *Server) getAttach(req *types.AttachRequest) (*types.AttachResponse, error) {
	return s.stream.streamServer.GetAttach(req)
}

// getPortForward returns port forward stream request
func (s *Server) getPortForward(req *types.PortForwardRequest) (*types.PortForwardResponse, error) {
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
		log.Warnf(ctx, "Could not read containers and sandboxes: %v", err)
	}
	pods := map[string]*storage.RuntimeContainerMetadata{}
	podContainers := map[string]*storage.RuntimeContainerMetadata{}
	names := map[string][]string{}
	deletedPods := map[string]*sandbox.Sandbox{}
	for i := range containers {
		metadata, err2 := s.StorageRuntimeServer().GetContainerMetadata(containers[i].ID)
		if err2 != nil {
			log.Warnf(ctx, "Error parsing metadata for %s: %v, ignoring", containers[i].ID, err2)
			continue
		}
		if !storage.IsCrioContainer(&metadata) {
			log.Debugf(ctx, "Container %s determined to not be a CRI-O container or sandbox", containers[i].ID)
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
		sb, err := s.LoadSandbox(ctx, sbID)
		if err == nil {
			continue
		}
		log.Warnf(ctx, "Could not restore sandbox %s: %v", sbID, err)
		for _, n := range names[sbID] {
			if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
				log.Warnf(ctx, "Unable to delete container %s: %v", n, err)
			}
			// Release the infra container name and the pod name for future use
			if strings.Contains(n, types.InfraContainerName) {
				s.ReleaseContainerName(n)
			} else {
				s.ReleasePodName(n)
			}
		}
		// Go through the containers and delete any container that was under the deleted pod
		log.Warnf(ctx, "Deleting all containers under sandbox %s since it could not be restored", sbID)
		for k, v := range podContainers {
			if v.PodID != sbID {
				continue
			}
			for _, n := range names[k] {
				if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
					log.Warnf(ctx, "Unable to delete container %s: %v", n, err)
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
		err := s.LoadContainer(ctx, containerID)
		if err == nil || err == lib.ErrIsNonCrioContainer {
			delete(containersAndTheirImages, containerID)
			continue
		}
		log.Warnf(ctx, "Could not restore container %s: %v", containerID, err)
		for _, n := range names[containerID] {
			if err := s.Store().DeleteContainer(n); err != nil && err != storageTypes.ErrNotAContainer {
				log.Warnf(ctx, "Unable to delete container %s: %v", n, err)
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
				log.Infof(ctx, "Successfully cleaned up network for pod %s", sb.ID())
			}
			return err
		}
		wipeResourceCleaner.Add(ctx, "cleanup sandbox network", cleanupFunc)
	}

	// If any failed to be deleted, the networking plugin is likely not ready.
	// The cleanup should be retried until it succeeds.
	go func() {
		if err := wipeResourceCleaner.Cleanup(); err != nil {
			log.Errorf(ctx, "Cleanup during server startup failed: %v", err)
		}
	}()

	// Restore sandbox IPs
	for _, sb := range s.ListSandboxes() {
		ips, err := s.getSandboxIPs(sb)
		if err != nil {
			log.Warnf(ctx, "Could not restore sandbox IP for %v: %v", sb.ID(), err)
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
		log.Debugf(ctx, "Shutting down all sandboxes, on shutdown")
		s.stopAllPodSandboxes(ctx)
		err = os.Remove(shutdownFile)
		if err != nil {
			log.Warnf(ctx, "Failed to remove %q", shutdownFile)
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

	if err := s.ContainerServer.Shutdown(); err != nil {
		return err
	}

	// first, make sure we sync all storage changes
	if err := utils.Sync(s.Store().GraphRoot()); err != nil {
		return errors.Wrapf(err, "failed to sync graph root after shutting down")
	}

	if s.config.CleanShutdownFile != "" {
		// then, we write the CleanShutdownFile
		// we do this after the sync, to ensure ordering.
		// Otherwise, we may run into situations where the CleanShutdownFile
		// is written before storage, causing us to think a corrupted storage
		// is not so.
		f, err := os.Create(s.config.CleanShutdownFile)
		if err != nil {
			return errors.Wrapf(err, "failed to write file to indicate a clean shutdown")
		}
		f.Close()

		// finally, attempt to sync the newly created file to disk.
		// It's still possible we crash after Create but before this Sync,
		// which will lead us to think storage wasn't synced.
		// However, that's much less likely than if we don't have a second Sync,
		// and less risky than if we don't Sync after the Create
		if err := utils.SyncParent(s.config.CleanShutdownFile); err != nil {
			return errors.Wrapf(err, "failed to sync clean shutdown file")
		}
	}

	return nil
}

// configureMaxThreads sets the Go runtime max threads threshold
// which is 90% of the kernel setting from /proc/sys/kernel/threads-max
func configureMaxThreads() error {
	mt, err := ioutil.ReadFile("/proc/sys/kernel/threads-max")
	if err != nil {
		return errors.Wrap(err, "read max threads file")
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

	err = runtimehandlerhooks.RestoreIrqBalanceConfig(config.IrqBalanceConfigFile, runtimehandlerhooks.IrqBannedCPUConfigFile, runtimehandlerhooks.IrqSmpAffinityProcFile)
	if err != nil {
		return nil, err
	}

	hostportManager := hostport.NewMetaHostportManager()

	// give server a tracer name to tag spans with
	tracerName, err := os.Hostname()
	if err != nil {
		return nil, err
	}

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
		tracerName:               tracerName,
		config:                   *config,
		monitorsChan:             make(chan struct{}),
		defaultIDMappings:        idMappings,
		pullOperationsInProgress: make(map[pullArguments]*pullOperation),
		resourceStore:            resourcestore.New(),
	}

	if err := configureMaxThreads(); err != nil {
		return nil, err
	}

	// Close stdin, so shortnames will not prompt
	devNullFile, err := os.Open(os.DevNull)
	if err != nil {
		return nil, errors.Wrap(err, "open devnull file")
	}

	defer devNullFile.Close()
	if err := unix.Dup2(int(devNullFile.Fd()), int(os.Stdin.Fd())); err != nil {
		return nil, errors.Wrap(err, "close stdin")
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
	s.stream.streamServer, err = streaming.NewServer(ctx, streamServerConfig, s.stream)
	if err != nil {
		return nil, fmt.Errorf("unable to create streaming server")
	}

	s.stream.streamServerCloseCh = make(chan struct{})
	go func() {
		defer close(s.stream.streamServerCloseCh)
		if err := s.stream.streamServer.Start(true); err != nil && err != http.ErrServerClosed {
			log.Fatalf(ctx, "Failed to start streaming server: %v", err)
		}
	}()

	log.Debugf(ctx, "Sandboxes: %v", s.ContainerServer.ListSandboxes())

	// Start a configuration watcher for the default config
	s.config.StartWatcher()

	// Start the metrics server if configured to be enabled
	if s.config.EnableMetrics {
		if err := metrics.New(&s.config.MetricsConfig).Start(s.monitorsChan); err != nil {
			return nil, err
		}
	} else {
		logrus.Debug("Metrics are disabled")
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
		log.Warnf(ctx, "Error encountered when checking whether cri-o should wipe images: %v", err)
	}

	// Note: some of these will fail if some aspect of the pod cleanup failed as well,
	// but this is best-effort anyway, as the Kubelet will eventually cleanup images when
	// disk usage gets too high.
	if shouldWipeImages {
		for _, img := range imagesToDelete {
			if err := s.removeImage(ctx, img); err != nil {
				log.Warnf(ctx, "Failed to remove image %s: %v", img, err)
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
	if !sb.Created() {
		return nil, errSandboxNotCreated
	}
	return sb, nil
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
func (s *Server) StartExitMonitor(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf(ctx, "Failed to create new watch: %v", err)
	}
	defer watcher.Close()

	done := make(chan struct{})
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				log.Debugf(ctx, "Event: %v", event)
				if event.Op&fsnotify.Create == fsnotify.Create {
					containerID := filepath.Base(event.Name)
					log.Debugf(ctx, "Container or sandbox exited: %v", containerID)
					c := s.GetContainer(containerID)
					if c != nil {
						log.Debugf(ctx, "Container exited and found: %v", containerID)
						err := s.Runtime().UpdateContainerStatus(ctx, c)
						if err != nil {
							log.Warnf(ctx, "Failed to update container status %s: %v", containerID, err)
						} else if err := s.ContainerStateToDisk(ctx, c); err != nil {
							log.Warnf(ctx, "Unable to write containers %s state to disk: %v", c.ID(), err)
						}
					} else {
						sb := s.GetSandbox(containerID)
						if sb != nil {
							c := sb.InfraContainer()
							if c == nil {
								log.Warnf(ctx, "No infra container set for sandbox: %v", containerID)
								continue
							}
							log.Debugf(ctx, "Sandbox exited and found: %v", containerID)
							err := s.Runtime().UpdateContainerStatus(ctx, c)
							if err != nil {
								log.Warnf(ctx, "Failed to update sandbox infra container status %s: %v", c.ID(), err)
							} else if err := s.ContainerStateToDisk(ctx, c); err != nil {
								log.Warnf(ctx, "Unable to write containers %s state to disk: %v", c.ID(), err)
							}
						}
					}
				}
			case err := <-watcher.Errors:
				log.Debugf(ctx, "Watch error: %v", err)
				close(done)
				return
			case <-s.monitorsChan:
				log.Debugf(ctx, "Closing exit monitor...")
				close(done)
				return
			}
		}
	}()
	if err := watcher.Add(s.config.ContainerExitsDir); err != nil {
		log.Errorf(ctx, "Watcher.Add(%q) failed: %s", s.config.ContainerExitsDir, err)
		close(done)
	}
	<-done
}
