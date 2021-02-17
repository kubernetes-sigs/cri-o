package sandbox_test

import (
	"os"
	"path/filepath"
	"time"

	"github.com/containers/storage/pkg/idtools"
	"github.com/cri-o/cri-o/internal/config/nsmgr"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/config"
	sandboxmock "github.com/cri-o/cri-o/test/mocks/sandbox"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	allManagedNamespaces = []nsmgr.NSType{
		nsmgr.NETNS, nsmgr.IPCNS, nsmgr.UTSNS, nsmgr.USERNS,
	}
	numManagedNamespaces = 4

	ids = []idtools.IDMap{
		{
			ContainerID: 0,
			HostID:      0,
			Size:        1000000,
		},
	}
	idMappings = idtools.NewIDMappingsFromMaps(ids, ids)
)

// pinNamespaceFunctor is a way to generically create a mockable pinNamespaces() function
// it stores a function that is used to populate the mock instance, which allows us to test
// different paths
type pinNamespacesFunctor struct {
	ifaceModifyFunc func(ifaceMock *sandboxmock.MockNamespaceIface)
}

// pinNamespaces is a spoof of namespaces_linux.go:pinNamespaces.
// it calls ifaceModifyFunc() to customize the behavior of this functor
func (p *pinNamespacesFunctor) pinNamespaces(nsTypes []nsmgr.NSType, cfg *config.Config, mappings *idtools.IDMappings, sysctls map[string]string) ([]sandbox.NamespaceIface, error) {
	ifaces := make([]sandbox.NamespaceIface, 0)
	for _, nsType := range nsTypes {
		if mappings == nil && nsType == nsmgr.USERNS {
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
func setPathToDir(directory string, ifaceMock *sandboxmock.MockNamespaceIface) nsmgr.NSType {
	// to be able to retrieve this value here, we need to burn one of
	// our allocated Type() calls. Luckily, we can just repopulate it immediately
	nsType := ifaceMock.Type()
	ifaceMock.EXPECT().Type().Return(nsType)

	ifaceMock.EXPECT().Path().Return(filepath.Join(directory, string(nsType)))
	return nsType
}

// The actual test suite
var _ = t.Describe("SandboxManagedNamespaces", func() {
	// Setup the SUT
	BeforeEach(beforeEach)
	t.Describe("CreateSandboxNamespaces", func() {
		It("should succeed if empty", func() {
			// Given
			managedNamespaces := make([]nsmgr.NSType, 0)

			// When
			ns, err := testSandbox.CreateManagedNamespaces(managedNamespaces, idMappings, nil, nil)

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
			managedNamespaces := []nsmgr.NSType{"invalid"}

			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, withRemoval.pinNamespaces)

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
			createdNamespaces, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, nil, successful.pinNamespaces)

			// Then
			Expect(err).To(BeNil())
			Expect(len(createdNamespaces)).To(Equal(numManagedNamespaces))
			for _, ns := range createdNamespaces {
				_, found := nsFound[ns.Path()]
				Expect(found).To(Equal(true))
			}
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
			withTmpDir := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := ifaceMock.Type()
					ifaceMock.EXPECT().Type().Return(nsType)
					ifaceMock.EXPECT().Path().Return(filepath.Join(tmpDir, string(nsType)))
					ifaceMock.EXPECT().Remove().Return(nil)
				},
			}

			createdNamespaces, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, nil, withTmpDir.pinNamespaces)
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
			managedNamespaces := []nsmgr.NSType{"net"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.NetNsJoin("/proc/self/ns/net")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has ipc namespace", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"ipc"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.IpcNsJoin("/proc/self/ns/ipc")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has uts namespace", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"uts"}

			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, successful.pinNamespaces)
			Expect(err).To(BeNil())
			err = testSandbox.UtsNsJoin("/proc/self/ns/uts")

			// Then
			Expect(err).NotTo(BeNil())
		})
		It("should fail when sandbox already has user namespace", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"user"}
			successful := newGenericFunctor()
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, successful.pinNamespaces)
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
			managedNamespaces := []nsmgr.NSType{"net"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.NetNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when ipc is set", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"ipc"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.IpcNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when uts is set", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"uts"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, getPath.pinNamespaces)
			Expect(err).To(BeNil())

			// When
			path := testSandbox.UtsNsPath()
			// Then
			Expect(path).ToNot(Equal(""))
		})
		It("should get something when user is set", func() {
			// Given
			managedNamespaces := []nsmgr.NSType{"user"}
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}

			_, err := testSandbox.CreateNamespacesWithFunc(managedNamespaces, idMappings, nil, nil, getPath.pinNamespaces)
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
			setupInfraContainerWithPid(1)
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
			// max valid pid is 4194304
			setupInfraContainerWithPid(4194305)
			// When
			nsPaths := testSandbox.NamespacePaths()
			// Then
			Expect(len(nsPaths)).To(Equal(0))
			Expect(testSandbox.PidNsPath()).To(BeEmpty())
		})
		It("should get managed path (except pid) despite infra set", func() {
			// Given
			setupInfraContainerWithPid(1)
			getPath := pinNamespacesFunctor{
				ifaceModifyFunc: func(ifaceMock *sandboxmock.MockNamespaceIface) {
					nsType := setPathToDir(genericNamespaceParentDir, ifaceMock)
					ifaceMock.EXPECT().Get().Return(&sandbox.Namespace{})
					ifaceMock.EXPECT().Path().Return(filepath.Join(genericNamespaceParentDir, string(nsType)))
				},
			}
			// When
			_, err := testSandbox.CreateNamespacesWithFunc(allManagedNamespaces, idMappings, nil, nil, getPath.pinNamespaces)
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

func setupInfraContainerWithPid(pid int) {
	testContainer, err := oci.NewContainer("testid", "testname", "",
		"/container/logs", map[string]string{},
		map[string]string{}, map[string]string{}, "image",
		"imageName", "imageRef", &oci.Metadata{},
		"testsandboxid", false, false, false, "",
		"/root/for/container", time.Now(), "SIGKILL")
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
