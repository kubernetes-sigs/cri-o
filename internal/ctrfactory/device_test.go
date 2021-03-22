package ctrfactory_test

import (
	"github.com/cri-o/cri-o/server/cri/types"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/opencontainers/runc/libcontainer/devices"
)

var _ = t.Describe("ContainerFactory", func() {
	t.Describe("SpecAddDevice", func() {
		type testdata struct {
			testDescription              string
			privileged                   bool
			privilegedWithoutHostDevices bool
			expectHostDevices            bool
		}
		hostDevices, err := devices.HostDevices()
		Expect(err).To(BeNil())

		tests := []testdata{
			{
				testDescription:              "Expect no host devices for non-privileged container",
				privileged:                   false,
				privilegedWithoutHostDevices: false,
				expectHostDevices:            false,
			},
			{
				testDescription:              "Expect no host devices for non-privileged container when privilegedWithoutHostDevices is true",
				privileged:                   false,
				privilegedWithoutHostDevices: true,
				expectHostDevices:            false,
			},
			{
				testDescription:              "Expect host devices for privileged container",
				privileged:                   true,
				privilegedWithoutHostDevices: false,
				expectHostDevices:            true,
			},
			{
				testDescription:              "Expect no host devices for privileged container when privilegedWithoutHostDevices is true",
				privileged:                   true,
				privilegedWithoutHostDevices: true,
				expectHostDevices:            false,
			},
		}

		for _, test := range tests {
			test := test
			It(test.testDescription, func() {
				// Given
				config := &types.ContainerConfig{
					Metadata: &types.ContainerMetadata{Name: "name"},
					Linux: &types.LinuxContainerConfig{
						SecurityContext: &types.LinuxContainerSecurityContext{
							Privileged: test.privileged,
						},
					},
					Devices: []*types.Device{},
				}
				sboxConfig := &types.PodSandboxConfig{
					Linux: &types.LinuxPodSandboxConfig{
						SecurityContext: &types.LinuxSandboxSecurityContext{
							Privileged: test.privileged,
						},
					},
				}
				Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())
				Expect(sut.SetPrivileged()).To(BeNil())
				Expect(sut.Privileged()).To(Equal(test.privileged))
				Expect(len(hostDevices)).NotTo(Equal(0))

				// When
				err := sut.SpecAddDevices(nil, nil, test.privilegedWithoutHostDevices)
				// Then
				Expect(err).To(BeNil())

				if !test.expectHostDevices {
					Expect(len(sut.Spec().Config.Linux.Devices)).To(Equal(0))
				} else {
					Expect(len(sut.Spec().Config.Linux.Devices)).To(Equal(len(hostDevices)))
				}
			})
		}
	})
})
