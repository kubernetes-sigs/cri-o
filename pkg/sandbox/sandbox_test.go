package sandbox_test

import (
	"fmt"
	"path/filepath"

	"github.com/containers/libpod/v2/pkg/annotations"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/server/cri/types"
	json "github.com/json-iterator/go"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Sandbox", func() {
	t.Describe("SetConfig", func() {
		It("should succeed", func() {
			// Given
			config := &types.PodSandboxConfig{
				Metadata: &types.PodSandboxMetadata{Name: "name"},
			}

			// When
			err := sut.SetConfig(config)

			// Then
			Expect(err).To(BeNil())
			Expect(sut.Config()).To(Equal(config))
		})

		It("should fail with nil config", func() {
			// Given
			// When
			err := sut.SetConfig(nil)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(sut.Config()).To(BeNil())
		})

		It("should fail with empty config", func() {
			// Given
			config := &types.PodSandboxConfig{}

			// When
			err := sut.SetConfig(config)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(sut.Config()).To(BeNil())
		})

		It("should fail with an empty name", func() {
			// Given
			config := &types.PodSandboxConfig{
				Metadata: &types.PodSandboxMetadata{},
			}

			// When
			err := sut.SetConfig(config)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(sut.Config()).To(BeNil())
		})

		It("should fail with config already set", func() {
			// Given
			config := &types.PodSandboxConfig{
				Metadata: &types.PodSandboxMetadata{Name: "name"},
			}
			err := sut.SetConfig(config)
			Expect(err).To(BeNil())

			// When
			err = sut.SetConfig(config)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(sut.Config()).NotTo(BeNil())
		})
	})
	t.Describe("SpecAddAnnotations", func() {
		It("should set the spec annotations", func() {
			sandboxConfig := &types.PodSandboxConfig{
				Metadata: &types.PodSandboxMetadata{Name: "name"},
				Linux: &types.LinuxPodSandboxConfig{
					SecurityContext: &types.LinuxSandboxSecurityContext{
						Privileged:       true,
						NamespaceOptions: &types.NamespaceOption{},
					},
				},
			}
			err := sut.SetConfig(sandboxConfig)
			Expect(err).To(BeNil())

			testAnnotations := make(map[string]string, 1)
			sut.Spec().Config.Annotations = testAnnotations

			ips := make([]string, 2)
			err = sut.SpecAddAnnotations("testPauseImage", "testContainerName", "testShmPath", "testPrivileged", "testRuntimeHandler", "testResolvPath", "testHostname", "testStopSignal", "testCgroupParent", "testMountPoint", "testHostnamePath", "testCniResultJSON", "testCreated", ips, false, false)
			Expect(err).To(BeNil())

			metadata, err := json.Marshal(sut.Config().Metadata)
			Expect(err).To(BeNil())

			labels, err := json.Marshal(sut.Config().Labels)
			Expect(err).To(BeNil())

			kubeAnnotations, err := json.Marshal(sut.Config().Annotations)
			Expect(err).To(BeNil())

			logDir := sut.Config().LogDirectory
			logPath := filepath.Join(logDir, sut.ID()+".log")

			securityContext := sut.Config().Linux.SecurityContext
			nsOptsJSON, err := json.Marshal(securityContext.NamespaceOptions)
			Expect(err).To(BeNil())
			hostNetwork := securityContext.NamespaceOptions.Network == types.NamespaceModeNODE

			Expect(sut.Spec().Config.Annotations[annotations.Metadata]).To(Equal(string(metadata)))
			Expect(sut.Spec().Config.Annotations[annotations.Labels]).To(Equal(string(labels)))
			Expect(sut.Spec().Config.Annotations[annotations.Annotations]).To(Equal(string(kubeAnnotations)))
			Expect(sut.Spec().Config.Annotations[annotations.LogPath]).To(Equal(logPath))
			Expect(sut.Spec().Config.Annotations[annotations.Name]).To(Equal(sut.Name()))
			Expect(sut.Spec().Config.Annotations[annotations.Namespace]).To(Equal(sut.Config().Metadata.Namespace))
			Expect(sut.Spec().Config.Annotations[annotations.ContainerType]).To(Equal(annotations.ContainerTypeSandbox))
			Expect(sut.Spec().Config.Annotations[annotations.SandboxID]).To(Equal(sut.ID()))
			Expect(sut.Spec().Config.Annotations[annotations.Image]).To(Equal("testPauseImage"))
			Expect(sut.Spec().Config.Annotations[annotations.ContainerName]).To(Equal("testContainerName"))
			Expect(sut.Spec().Config.Annotations[annotations.ContainerID]).To(Equal(sut.ID()))
			Expect(sut.Spec().Config.Annotations[annotations.ShmPath]).To(Equal("testShmPath"))
			Expect(sut.Spec().Config.Annotations[annotations.PrivilegedRuntime]).To(Equal("testPrivileged"))
			Expect(sut.Spec().Config.Annotations[annotations.RuntimeHandler]).To(Equal("testRuntimeHandler"))
			Expect(sut.Spec().Config.Annotations[annotations.ResolvPath]).To(Equal("testResolvPath"))
			Expect(sut.Spec().Config.Annotations[annotations.HostName]).To(Equal("testHostname"))
			Expect(sut.Spec().Config.Annotations[annotations.NamespaceOptions]).To(Equal(string(nsOptsJSON)))
			Expect(sut.Spec().Config.Annotations[annotations.KubeName]).To(Equal(sut.Config().Metadata.Name))
			Expect(sut.Spec().Config.Annotations[annotations.HostNetwork]).To(Equal(fmt.Sprintf("%v", hostNetwork)))
			Expect(sut.Spec().Config.Annotations[annotations.ContainerManager]).To(Equal(lib.ContainerManagerCRIO))
			Expect(sut.Spec().Config.Annotations[annotations.Created]).To(Equal("testCreated"))
			Expect(sut.Spec().Config.Annotations[annotations.CgroupParent]).To(Equal("testCgroupParent"))
			Expect(sut.Spec().Config.Annotations[annotations.MountPoint]).To(Equal("testMountPoint"))
			Expect(sut.Spec().Config.Annotations[annotations.HostnamePath]).To(Equal("testHostnamePath"))
		})
	})
})
