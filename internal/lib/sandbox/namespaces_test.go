package sandbox_test

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/config"
	sandboxmock "github.com/cri-o/cri-o/test/mocks/sandbox"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

var (
	allManagedNamespaces = []sandbox.NSType{
		sandbox.NETNS, sandbox.IPCNS, sandbox.UTSNS, sandbox.USERNS,
	}
	numManagedNamespaces = 4
	ids                  = []idtools.IDMap{
		{
			ContainerID: 0,
			HostID:      0,
			Size:        1000000,
		},
	}
	idMappings = idtools.NewIDMappingsFromMaps(ids, ids)

	// max valid pid is 4194304
	neverRunningPid     = 4194305
	alwaysRunningPid    = 1
	pidLocationFilename = "pid-location"
)

// pinNamespaceFunctor is a way to generically create a mockable pinNamespaces() function
// it stores a function that is used to populate the mock instance, which allows us to test
// different paths
type pinNamespacesFunctor struct {
	ifaceModifyFunc func(ifaceMock *sandboxmock.MockNamespaceIface)
}

// pinNamespaces is a spoof of namespaces_linux.go:pinNamespaces.
// it calls ifaceModifyFunc() to customize the behavior of this functor
func (p *pinNamespacesFunctor) pinNamespaces(nsTypes []sandbox.NSType, cfg *config.Config, mappings *idtools.IDMappings) ([]sandbox.NamespaceIface, error) {
	ifaces := make([]sandbox.NamespaceIface, 0)
	for _, nsType := range nsTypes {
		if mappings == nil && nsType == sandbox.USERNS {
			continue
		}
		ifaceMock := sandboxmock.NewMockNamespaceIface(mockCtrl)
		// we always call initialize and type, as they're both called no matter what happens
		// in CreateManagedNamespaces()
		ifaceMock.EXPECT().Initialize().Return(ifaceMock)
		ifaceMock.EXPECT().Type().Return(nsType)

		p.ifaceModifyFunc(ifaceMock)
		ifaces = append(ifaces, ifaceMock)
	}
	return ifaces, nil
}

// pinPidNamespace spoofs the creation of a pinned pid namespace
func (p *pinNamespacesFunctor) pinPidNamespace(cfg *config.Config, path string) (sandbox.NamespaceIface, error) {
	ifaceMock := sandboxmock.NewMockNamespaceIface(mockCtrl)
	ifaceMock.EXPECT().Path().Return(path)
	p.ifaceModifyFunc(ifaceMock)
	return ifaceMock, nil
}

// genericNamespaceParentDir is used when we create a generic functor
// it should not have anything created in it, nor should it be removed.
var genericNamespaceParentDir = "/tmp"

// newGenericFunctor takes a namespace directory and returns a functor
// that only further populates the Path() call.
// useful for situations we expect CreateManagedNamespaces to succeed
// (perhaps while testing other functionality)
func newGenericFunctor() *pinNamespacesFunctor {
	return &pinNamespacesFunctor{
		ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
			setPathToDir(genericNamespaceParentDir, ifaceMock)
		},
	}
}

// setPathToDir sets the ifaceMock's path to a directory
// using the Type() already loaded in.
// it returns the nsType, in case the caller wants to set the Path again
func setPathToDir(directory string, ifaceMock *sandboxmock.MockNamespaceIface) sandbox.NSType {
	// to be able to retrieve this value here, we need to burn one of
	// our allocated Type() calls. Luckily, we can just repopulate it immediately
	nsType := ifaceMock.Type()
	ifaceMock.EXPECT().Type().Return(nsType)

	ifaceMock.EXPECT().Path().Return(filepath.Join(directory, string(nsType)))
	return nsType
}

func setupInfraContainerWithPid(pid int) {
	setupInfraContainerWithPidAndTmpDir(pid, "/root/for/container")
}

func setupInfraContainerWithPidAndTmpDir(pid int, tmpDir string) {
	testContainer, err := oci.NewContainer("testid", "testname", "",
		"/container/logs", map[string]string{},
		map[string]string{}, map[string]string{}, "image",
		"imageName", "imageRef", &pb.ContainerMetadata{},
		"testsandboxid", false, false, false, "",
		tmpDir, time.Now(), "SIGKILL")
	Expect(err).To(BeNil())
	Expect(testContainer).NotTo(BeNil())

	cstate := &oci.ContainerState{}
	cstate.State = specs.State{
		Pid: pid,
	}
	// eat error here because callers may send invalid pids to test against
	_ = cstate.SetInitPid(pid) // nolint:errcheck
	testContainer.SetState(cstate)

	Expect(testSandbox.SetInfraContainer(testContainer)).To(BeNil())
}

// The actual test suite
var _ = t.Describe("SandboxManagedNamespaces", func() {
	// Setup the SUT
	BeforeEach(beforeEach)
	t.Describe("CreateSandboxNamespaces", func() {
		It("should succeed if empty", func() {
			// Given
			managedNamespaces := make([]sandbox.NSType, 0)

			// When
			ns, err := testSandbox.CreateManagedNamespaces(managedNamespaces, nil, nil)

			// Then
			Expect(err).To(BeNil())
			Expect(len(ns)).To(Equal(0))
		})

		It("should fail on invalid namespace", func() {
			withRemoval := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					ifaceMock.EXPECT().Remove().Return(nil)
				},
			}

			// Given
			managedNamespaces := []sandbox.NSType{"invalid"}

			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, nil, nil, withRemoval.pinNamespaces)

			// Then
			Expect(err).To(Not(BeNil()))
		})
		It("should succeed with valid namespaces", func() {
			// Given
			nsFound := make(map[string]bool)
			for _, nsType := range allManagedNamespaces {
				nsFound[filepath.Join(genericNamespaceParentDir, string(nsType))] = false
			}
			successful := newGenericFunctor()
			// When
			createdNamespaces, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, successful.pinNamespaces)

			// Then
			Expect(err).To(BeNil())
			Expect(len(createdNamespaces)).To(Equal(numManagedNamespaces))
			for _, ns := range createdNamespaces {
				_, found := nsFound[ns.Path()]
				Expect(found).To(Equal(true))
			}
		})
	})
	t.Describe("CreateManagedPidNamespace", func() {
		var tmpDir string
		BeforeEach(func() {
			tmpDir = createTmpDir()
		})
		AfterEach(func() {
			Expect(testSandbox.RemoveManagedNamespaces()).To(BeNil())
			Expect(os.RemoveAll(tmpDir)).To(BeNil())
		})
		It("should fail when config is empty", func() {
			// Given
			// When
			err := testSandbox.CreateManagedPidNamespace(nil)
			// Then
			Expect(err).To(Not(BeNil()))
		})
		It("should fail when infra pid not configured", func() {
			// Given
			successful := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {},
			}
			// When
			err := testSandbox.CreateManagedPidNamespaceWithFunc(&config.Config{
				RuntimeConfig: config.RuntimeConfig{
					NamespacesDir: tmpDir,
				},
			}, successful.pinPidNamespace)
			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when infra pid not running", func() {
			// Given
			failed := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {},
			}
			setupInfraContainerWithPid(neverRunningPid)
			// When
			err := testSandbox.CreateManagedPidNamespaceWithFunc(&config.Config{
				RuntimeConfig: config.RuntimeConfig{
					NamespacesDir: tmpDir,
				},
			}, failed.pinPidNamespace)
			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when we can't write to infra dir", func() {
			// Given
			withRemoval := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					ifaceMock.EXPECT().Remove().Return(nil)
				},
			}

			setupInfraContainerWithPid(alwaysRunningPid)
			// When
			err := testSandbox.CreateManagedPidNamespaceWithFunc(&config.Config{
				RuntimeConfig: config.RuntimeConfig{
					NamespacesDir: tmpDir,
				},
			}, withRemoval.pinPidNamespace)
			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should succeed when infra pid running", func() {
			// Given
			failed := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					ifaceMock.EXPECT().Remove().Return(nil)
				},
			}
			setupInfraContainerWithPidAndTmpDir(alwaysRunningPid, tmpDir)
			// When
			err := testSandbox.CreateManagedPidNamespaceWithFunc(&config.Config{
				RuntimeConfig: config.RuntimeConfig{
					NamespacesDir: tmpDir,
				},
			}, failed.pinPidNamespace)
			// Then
			Expect(err).To(BeNil())
			_, err = os.Stat(filepath.Join(tmpDir, pidLocationFilename))
			Expect(err).To(BeNil())
		})
	})
	t.Describe("RemoveManagedNamespaces", func() {
		It("should succeed when namespaces nil", func() {
			// Given
			// When
			err := testSandbox.RemoveManagedNamespaces()

			// Then
			Expect(err).To(BeNil())
		})
		It("should succeed when namespaces not nil", func() {
			// Given
			tmpDir := createTmpDir()
			defer os.RemoveAll(tmpDir)
			withTmpDir := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := ifaceMock.Type()
					ifaceMock.EXPECT().Type().Return(nsType)
					ifaceMock.EXPECT().Path().Return(filepath.Join(tmpDir, string(nsType)))
					ifaceMock.EXPECT().Remove().Return(nil)
				},
			}

			createdNamespaces, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, withTmpDir.pinNamespaces)
			Expect(err).To(BeNil())

			for _, ns := range createdNamespaces {
				f, err := os.Create(ns.Path())
				f.Close()

				Expect(err).To(BeNil())
			}

			// When
			err = testSandbox.RemoveManagedNamespaces()

			// Then
			Expect(err).To(BeNil())
		})
	})
	t.Describe("*NsJoin", func() {
		It("should succeed when asked to join a network namespace", func() {
			// Given
			err := testSandbox.NetNsJoin("/proc/self/ns/net")

			// Then
			Expect(err).To(BeNil())
		})
		It("should succeed when asked to join a ipc namespace", func() {
			// Given
			err := testSandbox.IpcNsJoin("/proc/self/ns/ipc")

			// Then
			Expect(err).To(BeNil())
		})
		It("should succeed when asked to join a uts namespace", func() {
			// Given
			err := testSandbox.UtsNsJoin("/proc/self/ns/uts")

			// Then
			Expect(err).To(BeNil())
		})
		It("should succeed when asked to join a user namespace", func() {
			// Given
			err := testSandbox.UserNsJoin("/proc/self/ns/user")

			// Then
			Expect(err).To(BeNil())
		})
		It("should fail when network namespace not exists", func() {
			// Given
			// When
			err := testSandbox.NetNsJoin("path")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when uts namespace not exists", func() {
			// Given
			// When
			err := testSandbox.UtsNsJoin("path")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when ipc namespace not exists", func() {
			// Given
			// When
			err := testSandbox.IpcNsJoin("path")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when user namespace not exists", func() {
			// Given
			// When
			err := testSandbox.UserNsJoin("path")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has network namespace", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"net"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.NetNsJoin("/proc/self/ns/net")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has ipc namespace", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"ipc"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, nil, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.IpcNsJoin("/proc/self/ns/ipc")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has uts namespace", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"uts"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, nil, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.UtsNsJoin("/proc/self/ns/uts")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has user namespace", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"user"}
			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.UserNsJoin("/proc/self/ns/user")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when asked to join a non-namespace", func() {
			// Given
			// When
			err := testSandbox.NetNsJoin("/tmp")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when asked to join a non-namespace", func() {
			// Given

			// When
			err := testSandbox.IpcNsJoin("/tmp")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when asked to join a non-namespace", func() {
			// Given
			// When
			err := testSandbox.UtsNsJoin("/tmp")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when asked to join a non-namespace", func() {
			// Given
			// When
			err := testSandbox.UserNsJoin("/tmp")

			// Then
			Expect(err).NotTo(BeNil())
		})
	})
	t.Describe("PidNsJoin", func() {
		var tmpDir string
		BeforeEach(func() {
			tmpDir = createTmpDir()
		})
		AfterEach(func() {
			Expect(os.RemoveAll(tmpDir)).To(BeNil())
		})

		It("should fail to join pidns without infra initialized", func() {
			// Given
			err := testSandbox.PidNsJoin()

			// Then
			Expect(err).To(Equal(sandbox.ErrNamespaceNotManaged))
		})
		It("should fail when asked to join a non-namespace", func() {
			// Given
			setupInfraContainerWithPidAndTmpDir(alwaysRunningPid, tmpDir)
			Expect(ioutil.WriteFile(filepath.Join(tmpDir, pidLocationFilename), []byte("/tmp"), 0o644)).To(BeNil())

			// When
			err := testSandbox.PidNsJoin()

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has pid namespace", func() {
			// Given
			setupInfraContainerWithPidAndTmpDir(alwaysRunningPid, tmpDir)
			Expect(ioutil.WriteFile(filepath.Join(tmpDir, pidLocationFilename), []byte("/proc/self/ns/pid"), 0o644)).To(BeNil())

			// When
			Expect(testSandbox.PidNsJoin()).To(BeNil())

			// Then
			Expect(testSandbox.PidNsJoin()).NotTo(BeNil())
		})
		It("should succeed", func() {
			// Given
			setupInfraContainerWithPidAndTmpDir(alwaysRunningPid, tmpDir)
			Expect(ioutil.WriteFile(filepath.Join(tmpDir, pidLocationFilename), []byte("/proc/self/ns/pid"), 0o644)).To(BeNil())

			// When
			err := testSandbox.PidNsJoin()

			// Then
			Expect(err).To(BeNil())
		})
	})
	t.Describe("*NsPath", func() {
		It("should get nothing when network not set", func() {
			// Given
			// When
			ns := testSandbox.NetNsPath()
			// Then
			Expect(ns).To(Equal(""))
		})
		It("should get nothing when ipc not set", func() {
			// Given
			// When
			ns := testSandbox.IpcNsPath()
			// Then
			Expect(ns).To(Equal(""))
		})
		It("should get nothing when uts not set", func() {
			// Given
			// When
			ns := testSandbox.UtsNsPath()
			// Then
			Expect(ns).To(Equal(""))
		})
		It("should get nothing when uts not set", func() {
			// Given
			// When
			ns := testSandbox.UserNsPath()
			// Then
			Expect(ns).To(Equal(""))
		})
		It("should get nothing when pid not set", func() {
			// Given
			// When
			ns := testSandbox.PidNsPath()
			// Then
			Expect(ns).To(Equal(""))
		})
		It("should get something when network is set", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"net"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.NetNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when ipc is set", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"ipc"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.IpcNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when uts is set", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"uts"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.UtsNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when user is set", func() {
			// Given
			managedNamespaces := []sandbox.NSType{"user"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.UserNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
	})
	t.Describe("NamespacePaths with infra", func() {
		It("should get nothing when infra set but pid 0", func() {
			// Given
			setupInfraContainerWithPid(0)
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			Expect(len(nsPaths)).To(Equal(0))
			Expect(testSandbox.PidNsPath()).To(BeEmpty())
		})
		It("should get something when infra set and pid running", func() {
			// Given
			setupInfraContainerWithPid(alwaysRunningPid)
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			for _, ns := range nsPaths {
				Expect(ns.Path()).To(ContainSubstring("/proc"))
			}
			Expect(len(nsPaths)).To(Equal(numManagedNamespaces))
			Expect(testSandbox.PidNsPath()).To(ContainSubstring("/proc"))
		})
		It("should get nothing when infra set with pid not running", func() {
			// Given
			setupInfraContainerWithPid(neverRunningPid)
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			Expect(len(nsPaths)).To(Equal(0))
			Expect(testSandbox.PidNsPath()).To(BeEmpty())
		})
		It("should get managed path (except pid) despite infra set", func() {
			// Given
			setupInfraContainerWithPid(alwaysRunningPid)
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			for _, ns := range nsPaths {
				Expect(ns.Path()).NotTo(ContainSubstring("/proc"))
			}
			Expect(len(nsPaths)).To(Equal(numManagedNamespaces))
			Expect(testSandbox.PidNsPath()).To(ContainSubstring("/proc"))
		})
		It("should get managed pid path despite infra set", func() {
			// Given
			tmpDir := createTmpDir()
			defer os.RemoveAll(tmpDir)

			successful := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(tmpDir)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(tmpDir)
				},
			}

			setupInfraContainerWithPidAndTmpDir(alwaysRunningPid, tmpDir)
			err := testSandbox.CreateManagedPidNamespaceWithFunc(&config.Config{
				RuntimeConfig: config.RuntimeConfig{
					NamespacesDir: tmpDir,
				},
			}, successful.pinPidNamespace)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.PidNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
			Expect(testSandbox.PidNsPath()).NotTo(ContainSubstring("/proc"))
		})
	})
	t.Describe("NamespacePaths without infra", func() {
		It("should get nothing", func() {
			// Given
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			Expect(len(nsPaths)).To(Equal(0))
		})
	})
})
