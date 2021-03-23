package server_test

import (
	"context"

	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
)

// The actual test suite
var _ = t.Describe("ContainerStats", func() {
	// Prepare the sut
	BeforeEach(func() {
		beforeEach()
		setupSUT()
	})

	AfterEach(afterEach)

	t.Describe("ContainerStats", func() {
		It("should fail on invalid container", func() {
			// Given
			// When
			response, err := sut.ContainerStats(context.Background(),
				&types.ContainerStatsRequest{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})
	})
})

var _ = t.Describe("ContainerStatsList", func() {
	// Prepare the sut
	BeforeEach(func() {
		beforeEach()
		setupSUT()
	})

	AfterEach(afterEach)

	t.Describe("ContainerStatsList", func() {
		It("should succeed", func() {
			// Given
			addContainerAndSandbox()
			// short circuit the call to GraphDriver
			// it only logs a warning, and we aren't testing it here
			gomock.InOrder(
				storeMock.EXPECT().
					GraphDriver().
					Return(nil, errors.New("avoid mocking graph driver")),
			)
			// When
			response, err := sut.ListContainerStats(context.Background(),
				&types.ListContainerStatsRequest{})

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
			Expect(len(response.Stats)).To(Equal(1))
		})
		It("should filter stopped container", func() {
			// Given
			state := oci.ContainerState{}
			state.Status = oci.ContainerStateStopped
			testContainer.SetState(&state)
			addContainerAndSandbox()

			// When
			response, err := sut.ListContainerStats(context.Background(),
				&types.ListContainerStatsRequest{},
			)

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
			Expect(len(response.Stats)).To(Equal(0))
		})
		It("should filter by id", func() {
			// Given
			addContainerAndSandbox()

			// When
			response, err := sut.ListContainerStats(context.Background(),
				&types.ListContainerStatsRequest{
					Filter: &types.ContainerStatsFilter{
						ID: "invalid",
					},
				},
			)

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
			Expect(len(response.Stats)).To(Equal(0))
		})
	})
})
