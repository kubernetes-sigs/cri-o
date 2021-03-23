package container_test

import (
	"github.com/cri-o/cri-o/server/cri/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
)

var _ = t.Describe("Container", func() {
	var config *types.ContainerConfig
	var sboxConfig *types.PodSandboxConfig
	const defaultMounts = 6
	BeforeEach(func() {
		config = &types.ContainerConfig{
			Metadata: &types.ContainerMetadata{Name: "name"},
		}
		sboxConfig = &types.PodSandboxConfig{}
	})
	t.Describe("SpecAddMounts", func() {
		It("should add the mount to the spec", func() {
			sut.AddMount(&rspec.Mount{
				Destination: "test",
				Type:        "test",
				Source:      "test",
				Options:     []string{"test"},
			})
			sut.SpecAddMounts()
			Expect(len(sut.Spec().Mounts())).To(Equal(defaultMounts + 1))
		})
		It("should add only one copy to the spec", func() {
			sut.AddMount(&rspec.Mount{
				Destination: "test",
				Type:        "test",
				Source:      "test",
				Options:     []string{"test"},
			})
			sut.AddMount(&rspec.Mount{
				Destination: "test",
				Type:        "test",
				Source:      "test",
				Options:     []string{"test"},
			})
			sut.SpecAddMounts()
			Expect(len(sut.Spec().Mounts())).To(Equal(defaultMounts + 1))
		})
	})
})
