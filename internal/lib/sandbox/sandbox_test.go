package sandbox_test

import (
	"time"

	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/server/cri/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// The actual test suite
var _ = t.Describe("Sandbox", func() {
	// Setup the SUT
	BeforeEach(beforeEach)

	t.Describe("New", func() {
		It("should succeed", func() {
			// Given
			id := "id"
			namespace := "namespace"
			name := "name"
			kubeName := "kubeName"
			logDir := "logDir"
			labels := map[string]string{"a": "labelA", "b": "labelB"}
			annotations := map[string]string{"a": "annotA", "b": "annotB"}
			processLabel := "processLabel"
			mountLabel := "mountLabel"
			metadata := sandbox.Metadata{Name: name}
			shmPath := "shmPath"
			cgroupParent := "cgroupParent"
			privileged := true
			runtimeHandler := "runtimeHandler"
			resolvPath := "resolvPath"
			hostname := "hostname"
			portMappings := []*hostport.PortMapping{{}, {}}
			hostNetwork := false
			createdAt := time.Now()

			// When
			sandbox, err := sandbox.New(id, namespace, name, kubeName, logDir,
				labels, annotations, processLabel, mountLabel, &metadata,
				shmPath, cgroupParent, privileged, runtimeHandler,
				resolvPath, hostname, portMappings, hostNetwork, createdAt, "")

			// Then
			Expect(err).To(BeNil())
			Expect(sandbox).NotTo(BeNil())
			Expect(sandbox.ID()).To(Equal(id))
			Expect(sandbox.Namespace()).To(Equal(namespace))
			Expect(sandbox.Name()).To(Equal(name))
			Expect(sandbox.KubeName()).To(Equal(kubeName))
			Expect(sandbox.LogDir()).To(Equal(logDir))
			Expect(sandbox.Labels()).To(ConsistOf([]string{"labelA", "labelB"}))
			Expect(sandbox.Annotations()).To(ConsistOf([]string{"annotA", "annotB"}))
			Expect(sandbox.ProcessLabel()).To(Equal(processLabel))
			Expect(sandbox.MountLabel()).To(Equal(mountLabel))
			Expect(sandbox.Metadata().Name).To(Equal(name))
			Expect(sandbox.ShmPath()).To(Equal(shmPath))
			Expect(sandbox.CgroupParent()).To(Equal(cgroupParent))
			Expect(sandbox.Privileged()).To(Equal(privileged))
			Expect(sandbox.RuntimeHandler()).To(Equal(runtimeHandler))
			Expect(sandbox.ResolvPath()).To(Equal(resolvPath))
			Expect(sandbox.Hostname()).To(Equal(hostname))
			Expect(sandbox.PortMappings()).To(Equal(portMappings))
			Expect(sandbox.HostNetwork()).To(Equal(hostNetwork))
			Expect(sandbox.StopMutex()).NotTo(BeNil())
			Expect(sandbox.Containers()).NotTo(BeNil())
			Expect(sandbox.CreatedAt()).To(Equal(createdAt))
		})
	})

	t.Describe("SetSeccompProfilePath", func() {
		It("should succeed", func() {
			// Given
			newPath := "/some/path"
			Expect(testSandbox.SeccompProfilePath()).NotTo(Equal(newPath))

			// When
			testSandbox.SetSeccompProfilePath(newPath)

			// Then
			Expect(testSandbox.SeccompProfilePath()).To(Equal(newPath))
		})
	})

	t.Describe("AddIPs", func() {
		It("should succeed", func() {
			// Given
			newIPs := []string{"10.0.0.1"}
			Expect(testSandbox.IPs()).NotTo(Equal(newIPs))

			// When
			testSandbox.AddIPs(newIPs)

			// Then
			Expect(testSandbox.IPs()).To(Equal(newIPs))
		})
	})

	t.Describe("Stopped", func() {
		It("should succeed", func() {
			// Given
			Expect(testSandbox.Stopped()).To(BeFalse())

			// When
			testSandbox.SetStopped(false)

			// Then
			Expect(testSandbox.Stopped()).To(BeTrue())
		})
	})

	t.Describe("NetworkStopped", func() {
		It("should succeed", func() {
			// Given
			Expect(testSandbox.NetworkStopped()).To(BeFalse())

			// When
			Expect(testSandbox.SetNetworkStopped(false)).To(BeNil())

			// Then
			Expect(testSandbox.NetworkStopped()).To(BeTrue())
		})
	})

	t.Describe("DNSConfig", func() {
		It("should succeed", func() {
			// Given
			Expect(testSandbox.DNSConfig()).To(BeNil())
			dnsConfig := types.DNSConfig{
				Servers:  []string{"server1", "server2"},
				Searches: []string{"searche1", "searches"},
				Options:  []string{"option1", "option2"},
			}

			// When
			testSandbox.SetDNSConfig(&dnsConfig)

			// Then
			Expect(testSandbox.DNSConfig()).To(Equal(&dnsConfig))
		})
	})

	t.Describe("Created", func() {
		It("should succeed", func() {
			// Given
			Expect(testSandbox.Created()).To(BeFalse())

			// When
			testSandbox.SetCreated()

			// Then
			Expect(testSandbox.Created()).To(BeTrue())
		})
	})

	t.Describe("AddHostnamePath", func() {
		It("should succeed", func() {
			// Given
			newHostnamePath := "hostnamePath"
			Expect(testSandbox.HostnamePath()).NotTo(Equal(newHostnamePath))

			// When
			testSandbox.AddHostnamePath(newHostnamePath)

			// Then
			Expect(testSandbox.HostnamePath()).To(Equal(newHostnamePath))
		})
	})

	t.Describe("SetNamespaceOptions", func() {
		It("should succeed", func() {
			// Given
			newNamespaceOption := &types.NamespaceOption{
				Network: 1,
				Pid:     2,
				Ipc:     3,
			}
			Expect(testSandbox.NamespaceOptions()).NotTo(Equal(newNamespaceOption))

			// When
			testSandbox.SetNamespaceOptions(newNamespaceOption)

			// Then
			Expect(testSandbox.NamespaceOptions().Network).
				To(Equal(newNamespaceOption.Network))
			Expect(testSandbox.NamespaceOptions().Pid).
				To(Equal(newNamespaceOption.Pid))
			Expect(testSandbox.NamespaceOptions().Ipc).
				To(Equal(newNamespaceOption.Ipc))
		})
	})

	t.Describe("Container", func() {
		var testContainer *oci.Container

		BeforeEach(func() {
			var err error
			testContainer, err = oci.NewContainer("testid", "testname", "",
				"/container/logs", map[string]string{},
				map[string]string{}, map[string]string{}, "image",
				"imageName", "imageRef", &oci.Metadata{},
				"testsandboxid", false, false, false, "",
				"/root/for/container", time.Now(), "SIGKILL")
			Expect(err).To(BeNil())
			Expect(testContainer).NotTo(BeNil())
		})

		It("should succeed to add and remove a container", func() {
			// Given
			Expect(testSandbox.GetContainer(testContainer.Name())).To(BeNil())

			// When
			testSandbox.AddContainer(testContainer)

			// Then
			Expect(testSandbox.GetContainer(testContainer.Name())).
				To(Equal(testContainer))

			// And When
			testSandbox.RemoveContainer(testContainer)

			// Then
			Expect(testSandbox.GetContainer(testContainer.Name())).To(BeNil())
		})

		It("should succeed to add and remove an infra container", func() {
			// Given
			Expect(testSandbox.InfraContainer()).To(BeNil())

			// When
			err := testSandbox.SetInfraContainer(testContainer)

			// Then
			Expect(err).To(BeNil())
			Expect(testSandbox.InfraContainer()).To(Equal(testContainer))
			// while we have a sandbox, it does not have any valid namespaces
			Expect(testSandbox.UserNsPath()).To(Equal(""))
			Expect(testSandbox.NetNsPath()).To(Equal(""))
			Expect(testSandbox.UtsNsPath()).To(Equal(""))
			Expect(testSandbox.IpcNsPath()).To(Equal(""))

			// And When
			testSandbox.RemoveInfraContainer()

			// Then
			Expect(testSandbox.InfraContainer()).To(BeNil())
			Expect(testSandbox.UserNsPath()).To(Equal(""))
			Expect(testSandbox.NetNsPath()).To(Equal(""))
			Expect(testSandbox.UtsNsPath()).To(Equal(""))
			Expect(testSandbox.IpcNsPath()).To(Equal(""))
		})

		It("should fail add an infra container twice", func() {
			// Given
			Expect(testSandbox.InfraContainer()).To(BeNil())
			Expect(testSandbox.SetInfraContainer(testContainer)).To(BeNil())

			// When
			err := testSandbox.SetInfraContainer(testContainer)

			// Then
			Expect(err).NotTo(BeNil())
		})

		It("should fail to set a nil infra container", func() {
			// Given
			// When
			err := testSandbox.SetInfraContainer(nil)

			// Then
			Expect(err).NotTo(BeNil())
		})
	})
	t.Describe("NeedsInfra", func() {
		It("should not need when managing NS and NS mode NODE", func() {
			// Given
			manageNS := true
			newNamespaceOption := &types.NamespaceOption{
				Pid: types.NamespaceModeNODE,
			}

			// When
			testSandbox.SetNamespaceOptions(newNamespaceOption)

			// Then
			Expect(testSandbox.NeedsInfra(manageNS)).To(Equal(false))
		})

		It("should not need when managing NS and NS mode CONTAINER", func() {
			// Given
			manageNS := true
			newNamespaceOption := &types.NamespaceOption{
				Pid: types.NamespaceModeCONTAINER,
			}

			// When
			testSandbox.SetNamespaceOptions(newNamespaceOption)

			// Then
			Expect(testSandbox.NeedsInfra(manageNS)).To(Equal(false))
		})

		It("should need when namespace mode POD", func() {
			// Given
			manageNS := false
			newNamespaceOption := &types.NamespaceOption{
				Pid: types.NamespaceModePOD,
			}

			// When
			testSandbox.SetNamespaceOptions(newNamespaceOption)

			// Then
			Expect(testSandbox.NeedsInfra(manageNS)).To(Equal(true))
		})

		It("should need when not managing NS", func() {
			// Given
			manageNS := true
			newNamespaceOption := &types.NamespaceOption{
				Pid: types.NamespaceModeCONTAINER,
			}

			// When
			testSandbox.SetNamespaceOptions(newNamespaceOption)

			// Then
			Expect(testSandbox.NeedsInfra(manageNS)).To(Equal(false))
		})
	})
})
