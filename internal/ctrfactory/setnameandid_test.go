package ctrfactory_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/cri-o/cri-o/internal/ctrfactory"
	"github.com/cri-o/cri-o/server/cri/types"
)

// The actual test suite
var _ = t.Describe("ContainerFactory:SetNameAndID", func() {
	// Setup the SUT
	BeforeEach(func() {
	})

	It("should succeed", func() {
		// Given
		const (
			name      = "name"
			namespace = "namespace"
			uid       = "uid"
		)
		metadata := &types.PodSandboxMetadata{
			Name: name, UID: uid, Namespace: namespace,
		}
		setupContainerWithMetadata(metadata)

		// When
		err := sut.SetNameAndID()

		// Then
		Expect(err).To(BeNil())
		Expect(len(sut.ID())).To(Equal(64))
		Expect(sut.Name()).To(ContainSubstring(name))
		Expect(sut.Name()).To(ContainSubstring(namespace))
		Expect(sut.Name()).To(ContainSubstring(uid))
	})

	It("should succeed with empty sandbox metadata", func() {
		// Given
		metadata := &types.PodSandboxMetadata{}
		setupContainerWithMetadata(metadata)

		// When
		err := sut.SetNameAndID()

		// Then
		Expect(err).To(BeNil())
	})

	It("should fail with config nil", func() {
		// Given
		// When
		container, err := ctrfactory.New()
		Expect(err).To(BeNil())

		err = container.SetNameAndID()

		// Then
		Expect(container).ToNot(BeNil())
		Expect(err).NotTo(BeNil())
	})
})

func setupContainerWithMetadata(md *types.PodSandboxMetadata) {
	config := &types.ContainerConfig{
		Metadata: &types.ContainerMetadata{Name: "name"},
	}
	sboxConfig := &types.PodSandboxConfig{
		Metadata: md,
	}
	Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())
}
