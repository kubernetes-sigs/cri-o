package ctrfactory_test

import (
	"github.com/cri-o/cri-o/server/cri/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// The actual test suite
var _ = t.Describe("ContainerFactory:SetConfig", func() {
	It("should succeed", func() {
		// Given
		config := &types.ContainerConfig{
			Metadata: &types.ContainerMetadata{Name: "name"},
		}

		sboxConfig := &types.PodSandboxConfig{}

		// When
		err := sut.SetConfig(config, sboxConfig)

		// Then
		Expect(err).To(BeNil())
		Expect(sut.Config()).To(Equal(config))
		Expect(sut.SandboxConfig()).To(Equal(sboxConfig))
	})

	It("should fail with nil config", func() {
		// Given
		// When
		err := sut.SetConfig(nil, nil)

		// Then
		Expect(err).NotTo(BeNil())
		Expect(sut.Config()).To(BeNil())
	})

	It("should fail with empty config", func() {
		// Given
		config := &types.ContainerConfig{}

		// When
		err := sut.SetConfig(config, nil)

		// Then
		Expect(err).NotTo(BeNil())
		Expect(sut.Config()).To(BeNil())
	})

	It("should fail with empty name", func() {
		// Given
		config := &types.ContainerConfig{
			Metadata: &types.ContainerMetadata{},
		}

		// When
		err := sut.SetConfig(config, nil)

		// Then
		Expect(err).NotTo(BeNil())
		Expect(sut.Config()).To(BeNil())
	})

	It("should fail with already set config", func() {
		// Given
		config := &types.ContainerConfig{
			Metadata: &types.ContainerMetadata{Name: "name"},
		}
		sboxConfig := &types.PodSandboxConfig{}
		err := sut.SetConfig(config, sboxConfig)
		Expect(err).To(BeNil())

		// When
		err = sut.SetConfig(config, nil)

		// Then
		Expect(err).NotTo(BeNil())
		Expect(sut.Config()).To(Equal(config))
	})

	It("should fail with empty sandbox config", func() {
		// Given
		config := &types.ContainerConfig{
			Metadata: &types.ContainerMetadata{Name: "name"},
		}

		// Then
		err := sut.SetConfig(config, nil)
		Expect(err).NotTo(BeNil())
	})
})
