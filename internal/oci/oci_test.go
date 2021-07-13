package oci_test

import (
	"context"
	"io/ioutil"
	"os"

	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/annotations"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/criu"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The actual test suite
var _ = t.Describe("Oci", func() {
	t.Describe("New", func() {
		It("should succeed with default config", func() {
			// Given
			c, err := libconfig.DefaultConfig()
			Expect(err).To(BeNil())

			// When
			runtime := oci.New(c)

			// Then
			Expect(runtime).NotTo(BeNil())
		})
	})

	t.Describe("Oci", func() {
		// The system under test
		var sut *oci.Runtime

		// Test constants
		const (
			invalidRuntime     = "invalid"
			defaultRuntime     = "runc"
			usernsRuntime      = "userns"
			performanceRuntime = "high-performance"
			vmRuntime          = "kata"
		)
		runtimes := libconfig.Runtimes{
			defaultRuntime: {
				RuntimePath: "/bin/sh",
				RuntimeType: "",
				RuntimeRoot: "/run/runc",
			},
			invalidRuntime: {},
			usernsRuntime: {
				RuntimePath:        "/bin/sh",
				RuntimeType:        "",
				RuntimeRoot:        "/run/runc",
				AllowedAnnotations: []string{annotations.UsernsModeAnnotation},
			},
			performanceRuntime: {
				RuntimePath: "/bin/sh",
				RuntimeType: "",
				RuntimeRoot: "/run/runc",
				AllowedAnnotations: []string{
					annotations.CPULoadBalancingAnnotation,
					annotations.IRQLoadBalancingAnnotation,
					annotations.CPUQuotaAnnotation,
					annotations.OCISeccompBPFHookAnnotation,
				},
			},
			vmRuntime: {
				RuntimePath:                  "/usr/bin/containerd-shim-kata-v2",
				RuntimeType:                  "vm",
				RuntimeRoot:                  "/run/vc",
				PrivilegedWithoutHostDevices: true,
				RuntimeConfigPath:            "/opt/kata-containers/config.toml",
			},
		}

		BeforeEach(func() {
			var err error
			config, err = libconfig.DefaultConfig()
			Expect(err).To(BeNil())
			config.DefaultRuntime = defaultRuntime
			config.Runtimes = runtimes

			sut = oci.New(config)
			Expect(sut).NotTo(BeNil())
		})

		It("should succeed to retrieve the runtimes", func() {
			// Given
			// When
			result := sut.Runtimes()

			// Then
			Expect(result).To(Equal(runtimes))
		})

		It("should succeed to validate a runtime handler", func() {
			// Given
			// When
			handler, err := sut.ValidateRuntimeHandler(defaultRuntime)

			// Then
			Expect(err).To(BeNil())
			Expect(handler).To(Equal(runtimes[defaultRuntime]))
		})
		It("should return an OCI runtime type if none is set", func() {
			// Given
			// When
			runtimeType, err := sut.RuntimeType(defaultRuntime)

			// Then
			Expect(err).To(BeNil())
			Expect(runtimeType).To(Equal(""))
		})
		It("should return a VM runtime type when it is set", func() {
			// Given
			// When
			runtimeType, err := sut.RuntimeType(vmRuntime)

			// Then
			Expect(err).To(BeNil())
			Expect(runtimeType).To(Equal(libconfig.RuntimeTypeVM))
		})
		Context("FilterDisallowedAnnotations", func() {
			It("should succeed to filter disallowed annotation", func() {
				// Given
				testAnn := map[string]string{
					annotations.DevicesAnnotation:          "/dev",
					annotations.IRQLoadBalancingAnnotation: "true",
				}
				Expect(runtimes[performanceRuntime].ValidateRuntimeAllowedAnnotations()).To(BeNil())

				// When
				err := sut.FilterDisallowedAnnotations(performanceRuntime, testAnn)

				// Then
				Expect(err).To(BeNil())
				_, ok := testAnn[annotations.DevicesAnnotation]
				Expect(ok).To(Equal(false))

				_, ok = testAnn[annotations.IRQLoadBalancingAnnotation]
				Expect(ok).To(Equal(true))
			})
			It("should fail to filter disallowed annotation of unknown runtime", func() {
				// Given
				testAnn := map[string]string{}

				// When
				err := sut.FilterDisallowedAnnotations("invalid", testAnn)

				// Then
				Expect(err).NotTo(BeNil())
			})
		})

		It("PrivilegedWithoutHostDevices should be true when set", func() {
			// Given
			// When
			privileged, err := sut.PrivilegedWithoutHostDevices(vmRuntime)

			// Then
			Expect(err).To(BeNil())
			Expect(privileged).To(Equal(true))
		})
		It("PrivilegedWithoutHostDevices should be false when runtime invalid", func() {
			// Given
			// When
			privileged, err := sut.PrivilegedWithoutHostDevices(invalidRuntime)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(privileged).To(Equal(false))
		})
		It("PrivilegedWithoutHostDevices should be false when runtime is the default", func() {
			// Given
			// When
			privileged, err := sut.PrivilegedWithoutHostDevices(defaultRuntime)

			// Then
			Expect(err).To(BeNil())
			Expect(privileged).To(Equal(false))
		})
		It("CheckpointContainer should succeed", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			beforeEach(sandboxID)
			defer func() { os.RemoveAll("dump.log") }()
			config.Runtimes["runc"] = &libconfig.RuntimeHandler{
				RuntimePath: "/bin/true",
			}

			specgen := &specs.Spec{
				Version: "1.0.0",
				Process: &specs.Process{
					SelinuxLabel: "",
				},
				Linux: &specs.Linux{
					MountLabel: "",
				},
			}
			// When
			err := sut.CheckpointContainer(context.Background(), myContainer, specgen, false)

			// Then
			Expect(err).To(BeNil())
		})
		It("CheckpointContainer should fail", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			defer func() { os.RemoveAll("dump.log") }()
			beforeEach(sandboxID)
			config.Runtimes["runc"] = &libconfig.RuntimeHandler{
				RuntimePath: "/bin/false",
			}

			specgen := &specs.Spec{
				Version: "1.0.0",
				Process: &specs.Process{
					SelinuxLabel: "",
				},
				Linux: &specs.Linux{
					MountLabel: "",
				},
			}
			// When
			err := sut.CheckpointContainer(context.Background(), myContainer, specgen, true)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(Equal("configured runtime does not support checkpoint/restore"))
		})
		It("RestoreContainer should fail with destination sandbox detection", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			beforeEach(sandboxID)
			config.Runtimes["runc"] = &libconfig.RuntimeHandler{
				RuntimePath: "/bin/true",
			}

			specgen := &specs.Spec{
				Version: "1.0.0",
			}
			err := os.Mkdir("checkpoint", 0o700)
			Expect(err).To(BeNil())
			defer func() { os.RemoveAll("checkpoint") }()
			inventory, err := os.OpenFile("checkpoint/inventory.img", os.O_RDONLY|os.O_CREATE, 0o644)
			Expect(err).To(BeNil())
			inventory.Close()

			// When
			err = sut.RestoreContainer(context.Background(), myContainer, specgen, 42, "no-parent-cgroup-exists")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(Equal("failed to detect destination sandbox of to be restored container containerID"))
		})
		It("RestoreContainer should fail with destination sandbox detection", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			beforeEach("")
			specgen := &specs.Spec{
				Version: "1.0.0",
			}
			err := os.Mkdir("checkpoint", 0o700)
			Expect(err).To(BeNil())
			defer func() { os.RemoveAll("checkpoint") }()
			inventory, err := os.OpenFile("checkpoint/inventory.img", os.O_RDONLY|os.O_CREATE, 0o644)
			Expect(err).To(BeNil())
			inventory.Close()

			// When
			err = sut.RestoreContainer(context.Background(), myContainer, specgen, 42, "no-parent-cgroup-exists")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(Equal("failed to detect sandbox of to be restored container containerID"))
		})
		It("RestoreContainer should fail", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			beforeEach(sandboxID)
			config.Runtimes["runc"] = &libconfig.RuntimeHandler{
				RuntimePath: "/bin/true",
			}

			specgen := &specs.Spec{
				Version:     "1.0.0",
				Annotations: map[string]string{"io.kubernetes.cri-o.SandboxID": "sandboxID"},
				Linux: &specs.Linux{
					MountLabel: ".",
				},
				Process: &specs.Process{
					SelinuxLabel: "",
				},
			}
			myContainer.SetSpec(specgen)

			err := os.Mkdir("checkpoint", 0o700)
			Expect(err).To(BeNil())
			defer func() { os.RemoveAll("checkpoint") }()
			inventory, err := os.OpenFile("checkpoint/inventory.img", os.O_RDONLY|os.O_CREATE, 0o644)
			Expect(err).To(BeNil())
			inventory.Close()

			err = ioutil.WriteFile(
				"config.json",
				[]byte(
					`{"ociVersion": "1.0.0","annotations":`+
						`{"io.kubernetes.cri-o.SandboxID": "sandboxID"},`+
						`"linux": {"mountLabel": ""}}`,
				),
				0o644,
			)
			Expect(err).To(BeNil())
			defer func() { os.RemoveAll("config.json") }()

			config.Conmon = "/bin/true"

			// When
			err = sut.RestoreContainer(context.Background(), myContainer, specgen, 42, "no-parent-cgroup-exists")
			defer func() { os.RemoveAll("restore.log") }()

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("failed to add conmon to systemd sandbox cgroup"))
		})
		It("RestoreContainer should fail with missing inventory", func() {
			if !criu.CheckForCriu() {
				Skip("CRIU is missing or too old.")
			}
			// Given
			beforeEach(sandboxID)
			specgen := &specs.Spec{
				Version:     "1.0.0",
				Annotations: map[string]string{"io.kubernetes.cri-o.SandboxID": "sandboxID"},
				Linux: &specs.Linux{
					MountLabel: ".",
				},
			}
			// When
			err := sut.RestoreContainer(context.Background(), myContainer, specgen, 42, "no-parent-cgroup-exists")

			// Then
			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(Equal("A complete checkpoint for this container cannot be found, cannot restore: stat checkpoint/inventory.img: no such file or directory"))
		})
	})

	t.Describe("BuildContainerdBinaryName", func() {
		It("Simple binary name (containerd-shim-kata-v2)", func() {
			binaryName := oci.BuildContainerdBinaryName("containerd-shim-kata-v2")
			Expect(binaryName).To(Equal("containerd.shim.kata.v2"))
		})

		It("Full binary path with a simple binary name (/usr/bin/containerd-shim-kata-v2)", func() {
			binaryName := oci.BuildContainerdBinaryName("/usr/bin/containerd-shim-kata-v2")
			Expect(binaryName).To(Equal("/usr/bin/containerd.shim.kata.v2"))
		})

		It("Composed binary name (containerd-shim-kata-qemu-with-dax-support-v2)", func() {
			binaryName := oci.BuildContainerdBinaryName("containerd-shim-kata-qemu-with-dax-support-v2")
			Expect(binaryName).To(Equal("containerd.shim.kata-qemu-with-dax-support.v2"))
		})

		It("Full binary path with a composed binary name (/usr/bin/containerd-shim-kata-v2)", func() {
			binaryName := oci.BuildContainerdBinaryName("/usr/bin/containerd-shim-kata-qemu-with-dax-support-v2")
			Expect(binaryName).To(Equal("/usr/bin/containerd.shim.kata-qemu-with-dax-support.v2"))
		})
	})
})
