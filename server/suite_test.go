package server_test

import (
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
	"time"

	cstorage "github.com/containers/storage"
	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server"
	. "github.com/cri-o/cri-o/test/framework"
	imagetypesmock "github.com/cri-o/cri-o/test/mocks/containers/image/v5"
	containerstoragemock "github.com/cri-o/cri-o/test/mocks/containerstorage"
	criostoragemock "github.com/cri-o/cri-o/test/mocks/criostorage"
	libmock "github.com/cri-o/cri-o/test/mocks/lib"
	ocimock "github.com/cri-o/cri-o/test/mocks/oci"
	ocicnitypesmock "github.com/cri-o/cri-o/test/mocks/ocicni"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
)

// TestServer runs the created specs
func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunFrameworkSpecs(t, "Server")
}

var (
	libMock           *libmock.MockIface
	mockCtrl          *gomock.Controller
	serverConfig      *config.Config
	storeMock         *containerstoragemock.MockStore
	imageServerMock   *criostoragemock.MockImageServer
	runtimeServerMock *criostoragemock.MockRuntimeServer
	imageCloserMock   *imagetypesmock.MockImageCloser
	cniPluginMock     *ocicnitypesmock.MockCNIPlugin
	ociRuntimeMock    *ocimock.MockRuntimeImpl
	sut               *server.Server
	t                 *TestFramework
	testContainer     *oci.Container
	testManifest      []byte
	testPath          string
	testSandbox       *sandbox.Sandbox
	testStreamService server.StreamService

	emptyDir string
)

const (
	sandboxID   = "sandboxID"
	containerID = "containerID"
)

var _ = BeforeSuite(func() {
	t = NewTestFramework(NilFunc, NilFunc)
	t.Setup()

	// Setup the mocks
	mockCtrl = gomock.NewController(GinkgoT())
	libMock = libmock.NewMockIface(mockCtrl)
	storeMock = containerstoragemock.NewMockStore(mockCtrl)
	imageServerMock = criostoragemock.NewMockImageServer(mockCtrl)
	runtimeServerMock = criostoragemock.NewMockRuntimeServer(mockCtrl)
	imageCloserMock = imagetypesmock.NewMockImageCloser(mockCtrl)
	cniPluginMock = ocicnitypesmock.NewMockCNIPlugin(mockCtrl)
	ociRuntimeMock = ocimock.NewMockRuntimeImpl(mockCtrl)

	emptyDir = t.MustTempDir("crio-empty")
})

var _ = AfterSuite(func() {
	t.Teardown()
	mockCtrl.Finish()
})

var beforeEach = func() {
	// Only log panics for now
	logrus.SetLevel(logrus.PanicLevel)

	// Setup test data
	testManifest = []byte(`{
		"annotations": {
			"io.kubernetes.cri-o.Annotations": "{}",
			"io.kubernetes.cri-o.ContainerID": "sandboxID",
			"io.kubernetes.cri-o.ContainerName": "containerName",
			"io.kubernetes.cri-o.ContainerType": "{}",
			"io.kubernetes.cri-o.Created": "2006-01-02T15:04:05.999999999Z",
			"io.kubernetes.cri-o.HostName": "{}",
			"io.kubernetes.cri-o.CgroupParent": "{}",
			"io.kubernetes.cri-o.IP": "{}",
			"io.kubernetes.cri-o.NamespaceOptions": "{}",
			"io.kubernetes.cri-o.SeccompProfilePath": "{}",
			"io.kubernetes.cri-o.Image": "{}",
			"io.kubernetes.cri-o.ImageName": "{}",
			"io.kubernetes.cri-o.ImageRef": "{}",
			"io.kubernetes.cri-o.KubeName": "{}",
			"io.kubernetes.cri-o.PortMappings": "[]",
			"io.kubernetes.cri-o.Labels": "{}",
			"io.kubernetes.cri-o.LogPath": "{}",
			"io.kubernetes.cri-o.Metadata": "{}",
			"io.kubernetes.cri-o.Name": "name",
			"io.kubernetes.cri-o.Namespace": "default",
			"io.kubernetes.cri-o.PrivilegedRuntime": "{}",
			"io.kubernetes.cri-o.ResolvPath": "{}",
			"io.kubernetes.cri-o.HostnamePath": "{}",
			"io.kubernetes.cri-o.SandboxID": "sandboxID",
			"io.kubernetes.cri-o.SandboxName": "{}",
			"io.kubernetes.cri-o.ShmPath": "{}",
			"io.kubernetes.cri-o.MountPoint": "{}",
			"io.kubernetes.cri-o.TrustedSandbox": "{}",
			"io.kubernetes.cri-o.Stdin": "{}",
			"io.kubernetes.cri-o.StdinOnce": "{}",
			"io.kubernetes.cri-o.Volumes": "[{}]",
			"io.kubernetes.cri-o.HostNetwork": "{}",
			"io.kubernetes.cri-o.CNIResult": "{}"
		},
		"linux": {
			"namespaces": [
				{"type": "network", "path": "/proc/self/ns/net"}
			]
		},
		"process": {
			"selinuxLabel": "system_u:system_r:container_runtime_t:s0"
		}}`)

	// Prepare the server config
	var err error
	testPath, err = filepath.Abs("test")
	Expect(err).To(BeNil())
	serverConfig, err = config.DefaultConfig()
	Expect(err).To(BeNil())
	serverConfig.ContainerAttachSocketDir = testPath
	serverConfig.ContainerExitsDir = path.Join(testPath, "exits")
	serverConfig.LogDir = path.Join(testPath, "log")
	serverConfig.CleanShutdownFile = path.Join(testPath, "clean.shutdown")

	// We want a directory that is guaranteed to exist, but it must
	// be empty so we don't erroneously load anything and make tests
	// unreproducible.
	serverConfig.NetworkDir = emptyDir
	serverConfig.PluginDirs = []string{emptyDir}
	serverConfig.HooksDir = []string{emptyDir}

	// Initialize test container and sandbox
	testSandbox, err = sandbox.New(sandboxID, "", "", "", ".",
		make(map[string]string), make(map[string]string), "", "",
		&sandbox.Metadata{}, "", "", false, "", "", "",
		[]*hostport.PortMapping{}, false, time.Now(), "")
	Expect(err).To(BeNil())

	testContainer, err = oci.NewContainer(containerID, "", "", "",
		make(map[string]string), make(map[string]string),
		make(map[string]string), "pauseImage", "", "",
		&oci.Metadata{}, sandboxID, false, false,
		false, "", "", time.Now(), "")
	Expect(err).To(BeNil())

	// Initialize test streaming server
	streamServerConfig := streaming.DefaultConfig
	testStreamService = server.StreamService{}
	testStreamService.SetRuntimeServer(sut)
	server, err := streaming.NewServer(streamServerConfig, testStreamService)
	Expect(err).To(BeNil())
	Expect(server).NotTo(BeNil())
}

var afterEach = func() {
	os.RemoveAll(testPath)
	os.RemoveAll("state.json")
	os.RemoveAll("config.json")
}

var setupSUT = func() {
	var err error
	mockNewServer()
	sut, err = server.New(context.Background(), libMock)
	Expect(err).To(BeNil())
	Expect(sut).NotTo(BeNil())

	// Inject the mock
	sut.SetStorageImageServer(imageServerMock)
	sut.SetStorageRuntimeServer(runtimeServerMock)
	Expect(sut.SetCNIPlugin(cniPluginMock)).To(BeNil())
}

func mockNewServer() {
	gomock.InOrder(
		libMock.EXPECT().GetData().Times(2).Return(serverConfig),
		libMock.EXPECT().GetStore().Return(storeMock, nil),
		libMock.EXPECT().GetData().Return(serverConfig),
		storeMock.EXPECT().Containers().
			Return([]cstorage.Container{}, nil),
	)
}

func addContainerAndSandbox() {
	Expect(sut.AddSandbox(testSandbox)).To(BeNil())
	Expect(testSandbox.SetInfraContainer(testContainer)).To(BeNil())
	sut.AddContainer(testContainer)
	Expect(sut.CtrIDIndex().Add(testContainer.ID())).To(BeNil())
	Expect(sut.PodIDIndex().Add(testSandbox.ID())).To(BeNil())
	testContainer.SetCreated()
	testSandbox.SetCreated()
}

var mockDirs = func(manifest []byte) {
	gomock.InOrder(
		storeMock.EXPECT().
			FromContainerDirectory(gomock.Any(), gomock.Any()).
			Return(manifest, nil),
		storeMock.EXPECT().ContainerRunDirectory(gomock.Any()).
			Return("", nil),
		storeMock.EXPECT().ContainerDirectory(gomock.Any()).
			Return("", nil),
	)
}

func createDummyState() {
	Expect(ioutil.WriteFile("state.json", []byte(`{}`), 0o644)).To(BeNil())
}

func createDummyConfig() {
	Expect(ioutil.WriteFile("config.json", []byte(`{"linux":{},"process":{}}`), 0o644)).To(BeNil())
}

func mockRuncInLibConfig() {
	echo, err := exec.LookPath("echo")
	Expect(err).To(BeNil())
	serverConfig.Runtimes["runc"] = &config.RuntimeHandler{
		RuntimePath: echo,
	}
}
