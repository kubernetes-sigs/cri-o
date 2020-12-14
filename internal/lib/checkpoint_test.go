package lib_test

import (
	"context"
	"os"

	cstorage "github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/pkg/criu"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The actual test suite
var _ = t.Describe("ContainerCheckpoint", func() {
	// Prepare the sut
	BeforeEach(func() {
		beforeEach()
		createDummyConfig()
		mockRuncInLibConfig()
		if !criu.CheckForCriu() {
			Skip("CRIU is missing or too old.")
		}
	})

	AfterEach(func() {
		os.RemoveAll("dump.log")
	})

	t.Describe("ContainerCheckpoint", func() {
		It("should fail with container not running", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()

			opts.Container = containerID

			// When
			res, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(Equal(""))
			Expect(err.Error()).To(Equal(`container containerID is not running`))
		})
	})
	t.Describe("ContainerCheckpoint", func() {
		It("should succeed", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()
			opts.Container = containerID

			myContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			myContainer.SetSpec(&specs.Spec{Version: "1.0.0"})

			gomock.InOrder(
				storeMock.EXPECT().Container(gomock.Any()).Return(&cstorage.Container{}, nil),
				storeMock.EXPECT().Unmount(gomock.Any(), gomock.Any()).Return(true, nil),
			)

			// When
			res, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).To(BeNil())
			Expect(res).To(Equal(opts.Container))
		})
	})
	t.Describe("ContainerCheckpoint", func() {
		It("should fail because runtime failure (/bin/false)", func() {
			// Given
			mockRuncToFalseInLibConfig()
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()
			opts.Container = containerID

			myContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			myContainer.SetSpec(&specs.Spec{Version: "1.0.0"})

			// When
			_, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).ToNot(BeNil())
			Expect(err.Error()).To(ContainSubstring(`failed to checkpoint container containerID`))
		})
	})
	t.Describe("ContainerCheckpoint", func() {
		It("should fail with export", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()
			opts.Container = containerID
			opts.TargetFile = "cp.tar"
			defer func() { os.RemoveAll("cp.tar") }()

			myContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			myContainer.SetSpec(&specs.Spec{Version: "1.0.0"})

			gomock.InOrder(
				storeMock.EXPECT().Container(gomock.Any()).Return(&cstorage.Container{}, nil),
				storeMock.EXPECT().Changes(gomock.Any(), gomock.Any()).Return([]archive.Change{{Kind: archive.ChangeDelete, Path: "deleted.file"}}, nil),
				storeMock.EXPECT().Mount(gomock.Any(), gomock.Any()).Return("/tmp/", nil),
				storeMock.EXPECT().Container(gomock.Any()).Return(&cstorage.Container{}, nil),
				storeMock.EXPECT().Unmount(gomock.Any(), gomock.Any()).Return(true, nil),
			)

			// When
			res, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).To(BeNil())
			Expect(res).To(ContainSubstring(opts.Container))
		})
	})
	t.Describe("ContainerCheckpoint", func() {
		It("should fail during unmount", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()
			opts.Container = containerID

			myContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			myContainer.SetSpec(&specs.Spec{Version: "1.0.0"})

			gomock.InOrder(
				storeMock.EXPECT().Container(gomock.Any()).Return(&cstorage.Container{}, nil),
				storeMock.EXPECT().Unmount(gomock.Any(), gomock.Any()).Return(true, t.TestError),
			)

			// When
			_, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err.Error()).To(Equal(`failed to unmount container containerID: error`))
		})
	})
})

var _ = t.Describe("ContainerCheckpoint", func() {
	// Prepare the sut
	BeforeEach(beforeEach)

	t.Describe("ContainerCheckpoint", func() {
		It("should fail with invalid container ID", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions
			opts.Container = "invalid"

			// When
			res, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(Equal(""))
			Expect(err.Error()).To(Equal(`failed to find container invalid: container with ID starting with invalid not found: ID does not exist`))
		})
	})
	t.Describe("ContainerCheckpoint", func() {
		It("should fail with invalid config", func() {
			// Given
			var opts lib.ContainerCheckpointRestoreOptions

			addContainerAndSandbox()

			opts.Container = containerID

			// When
			res, err := sut.ContainerCheckpoint(context.Background(), &opts)

			// Then
			Expect(err).NotTo(BeNil())
			Expect(res).To(Equal(""))
			Expect(err.Error()).To(Equal(`Not able to read config for container "containerID": template configuration at config.json not found`))
		})
	})
})
