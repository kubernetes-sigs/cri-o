package container_test

import (
	"github.com/cri-o/cri-o/internal/config/apparmor"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

var _ = t.Describe("Container", func() {
	var config *pb.ContainerConfig
	var sboxConfig *pb.PodSandboxConfig
	BeforeEach(func() {
		config = &pb.ContainerConfig{
			Metadata: &pb.ContainerMetadata{Name: "name"},
		}
		sboxConfig = &pb.PodSandboxConfig{}
	})
	t.Describe("FipsDisable", func() {
		It("should be true when set to true", func() {
			// Given
			labels := make(map[string]string)
			labels["FIPS_DISABLE"] = "true"
			sboxConfig.Labels = labels

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.DisableFips()).To(Equal(true))
		})
		It("should be false when set to false", func() {
			// Given
			labels := make(map[string]string)
			labels["FIPS_DISABLE"] = "false"
			sboxConfig.Labels = labels

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.DisableFips()).To(Equal(false))
		})
		It("should be false when not set", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.DisableFips()).To(Equal(false))
		})
	})
	t.Describe("Image", func() {
		It("should fail when spec not set", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			img, err := sut.Image()
			Expect(err).NotTo(BeNil())
			Expect(img).To(BeEmpty())
		})
		It("should fail when image not set", func() {
			// Given
			config.Image = &pb.ImageSpec{}

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			img, err := sut.Image()
			Expect(err).NotTo(BeNil())
			Expect(img).To(BeEmpty())
		})
		It("should be succeed when set", func() {
			// Given
			testImage := "img"
			config.Image = &pb.ImageSpec{
				Image: testImage,
			}

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			img, err := sut.Image()
			Expect(err).To(BeNil())
			Expect(img).To(Equal(testImage))
		})
	})
	t.Describe("ReadOnly", func() {
		BeforeEach(func() {
			config.Linux = &pb.LinuxContainerConfig{
				SecurityContext: &pb.LinuxContainerSecurityContext{},
			}
		})
		It("should not be readonly by default", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.ReadOnly(false)).To(Equal(false))
		})
		It("should be readonly when specified", func() {
			// Given
			config.Linux.SecurityContext.ReadonlyRootfs = true

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.ReadOnly(false)).To(Equal(true))
		})
		It("should be readonly when server is", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			Expect(sut.ReadOnly(true)).To(Equal(true))
		})
	})
	t.Describe("SelinuxLabel", func() {
		BeforeEach(func() {
			config.Linux = &pb.LinuxContainerConfig{
				SecurityContext: &pb.LinuxContainerSecurityContext{},
			}
		})
		It("should be empty by default", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			labels, err := sut.SelinuxLabel("")
			Expect(labels).To(BeEmpty())
			Expect(err).To(BeNil())
		})
		It("should not be empty when specified in config", func() {
			// Given
			config.Linux.SecurityContext.SelinuxOptions = &pb.SELinuxOption{
				User:  "a_u",
				Role:  "a_r",
				Type:  "a_t",
				Level: "a_l",
			}

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			labels, err := sut.SelinuxLabel("")
			Expect(len(labels)).To(Equal(4))
			Expect(err).To(BeNil())
		})
		It("should not be empty when specified in sandbox", func() {
			// Given

			// When
			Expect(sut.SetConfig(config, sboxConfig)).To(BeNil())

			// Then
			labels, err := sut.SelinuxLabel("a_u:a_t:a_r")
			Expect(len(labels)).To(Equal(3))
			Expect(err).To(BeNil())
		})
	})

	t.Describe("ApplyAppArmor", func() {
		appArmorConfig := apparmor.New()

		It("should succeed with empty profile name", func() {
			// Given
			Expect(sut.InitSpecGen()).To(BeNil())

			// When
			err := sut.ApplyAppArmor(appArmorConfig)

			// Then
			Expect(err).To(BeNil())
			if appArmorConfig.IsEnabled() {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(Equal(apparmor.DefaultProfile))
			} else {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(BeEmpty())
			}
		})

		It("should succeed with provileged container", func() {
			// Given
			Expect(sut.InitSpecGen()).To(BeNil())
			Expect(sut.SetPrivileged()).To(BeNil())

			// When
			err := sut.ApplyAppArmor(apparmor.New())

			// Then
			Expect(err).To(BeNil())
			if appArmorConfig.IsEnabled() {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(Equal(apparmor.DefaultProfile))
			} else {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(BeEmpty())
			}
		})

		It("should succeed with custom profile name", func() {
			// Given
			const profileName = "my-profile"
			Expect(sut.InitSpecGen()).To(BeNil())
			sut.Config().Linux.SecurityContext.ApparmorProfile = profileName

			// When
			err := sut.ApplyAppArmor(appArmorConfig)

			// Then
			Expect(err).To(BeNil())
			if appArmorConfig.IsEnabled() {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(Equal(profileName))
			} else {
				Expect(sut.SpecGen().Config.Process.ApparmorProfile).
					To(BeEmpty())
			}
		})

		It("should fail without initialized spec", func() {
			// Given
			// When
			err := sut.ApplyAppArmor(apparmor.New())

			// Then
			Expect(err).NotTo(BeNil())
		})
	})
})
