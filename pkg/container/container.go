package container

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containers/storage/pkg/stringid"
	"github.com/cri-o/cri-o/internal/config/apparmor"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/utils"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// Container is the main public container interface
type Container interface {
	// All set methods are usually called in order of their definition

	// SetConfig sets the configuration to the container and validates it
	SetConfig(*pb.ContainerConfig, *pb.PodSandboxConfig) error

	// SetNameAndID sets a container name and ID
	SetNameAndID() error

	// Config returns the container CRI configuration
	Config() *pb.ContainerConfig

	// SandboxConfig returns the sandbox CRI configuration
	SandboxConfig() *pb.PodSandboxConfig

	// ID returns the container ID
	ID() string

	// Name returns the container name
	Name() string

	// SetPrivileged sets the privileged bool for the container
	SetPrivileged() error

	// Privileged returns whether this container is privileged
	Privileged() bool

	// LogPath returns the log path for the container
	// It takes as input the LogDir of the sandbox, which is used
	// if there is no LogDir configured in the sandbox CRI config
	LogPath(string) (string, error)

	// DisableFips returns whether the container should disable fips mode
	DisableFips() bool

	// Image returns the image specified in the container spec, or an error
	Image() (string, error)

	// ReadOnly returns whether the rootfs should be readonly
	// it takes a bool as to whether crio was configured to
	// be readonly, which it defaults to if the container wasn't
	// specifically asked to be read only
	ReadOnly(bool) bool

	// SelinuxLabel returns the container's SelinuxLabel
	// it takes the sandbox's label, which it falls back upon
	SelinuxLabel(string) ([]string, error)

	// InitSpecGen initializes a new config spec generator.
	InitSpecGen() error

	// SpecGen returns the internal config spec generator.
	SpecGen() *generate.Generator

	// ApplyAppArmor takes the AppArmor config and applies it to the internal
	// config spec generator.
	ApplyAppArmor(*apparmor.Config) error
}

// container is the hidden default type behind the Container interface
type container struct {
	ctx        context.Context
	config     *pb.ContainerConfig
	sboxConfig *pb.PodSandboxConfig
	id         string
	name       string
	privileged bool
	specGen    *generate.Generator
}

// New creates a new, empty Sandbox instance
func New(ctx context.Context) Container {
	return &container{
		ctx: ctx,
	}
}

// SetConfig sets the configuration to the container and validates it
func (c *container) SetConfig(config *pb.ContainerConfig, sboxConfig *pb.PodSandboxConfig) error {
	if c.config != nil {
		return errors.New("config already set")
	}

	if config == nil {
		return errors.New("config is nil")
	}

	if config.GetMetadata() == nil {
		return errors.New("metadata is nil")
	}

	if config.GetMetadata().GetName() == "" {
		return errors.New("name is nil")
	}

	if sboxConfig == nil {
		return errors.New("sandbox config is nil")
	}

	if c.sboxConfig != nil {
		return errors.New("sandbox config is already set")
	}

	c.config = config
	c.sboxConfig = sboxConfig
	return nil
}

// SetNameAndID sets a container name and ID
func (c *container) SetNameAndID() error {
	if c.config == nil {
		return errors.New("config is not set")
	}

	if c.sboxConfig == nil {
		return errors.New("sandbox config is nil")
	}

	if c.sboxConfig.Metadata == nil {
		return errors.New("sandbox metadata is nil")
	}

	id := stringid.GenerateNonCryptoID()
	name := strings.Join([]string{
		"k8s",
		c.config.Metadata.Name,
		c.sboxConfig.Metadata.Name,
		c.sboxConfig.Metadata.Namespace,
		c.sboxConfig.Metadata.Uid,
		fmt.Sprintf("%d", c.config.Metadata.Attempt),
	}, "_")

	c.id = id
	c.name = name
	return nil
}

// Config returns the container configuration
func (c *container) Config() *pb.ContainerConfig {
	return c.config
}

// SandboxConfig returns the sandbox configuration
func (c *container) SandboxConfig() *pb.PodSandboxConfig {
	return c.sboxConfig
}

// ID returns the container ID
func (c *container) ID() string {
	return c.id
}

// Name returns the container name
func (c *container) Name() string {
	return c.name
}

// SetPrivileged sets the privileged bool for the container
func (c *container) SetPrivileged() error {
	if c.config == nil {
		return nil
	}
	if c.config.GetLinux() == nil {
		return nil
	}
	if c.config.GetLinux().GetSecurityContext() == nil {
		return nil
	}

	if c.sboxConfig == nil {
		return nil
	}

	if c.sboxConfig.GetLinux() == nil {
		return nil
	}

	if c.sboxConfig.GetLinux().GetSecurityContext() == nil {
		return nil
	}

	if c.config.GetLinux().GetSecurityContext().GetPrivileged() {
		if !c.sboxConfig.GetLinux().GetSecurityContext().GetPrivileged() {
			return errors.New("no privileged container allowed in sandbox")
		}
		c.privileged = true
	}
	return nil
}

// Privileged returns whether this container is privileged
func (c *container) Privileged() bool {
	return c.privileged
}

// LogPath returns the log path for the container
// It takes as input the LogDir of the sandbox, which is used
// if there is no LogDir configured in the sandbox CRI config
func (c *container) LogPath(sboxLogDir string) (string, error) {
	sboxLogDirConfig := c.sboxConfig.GetLogDirectory()
	if sboxLogDirConfig != "" {
		sboxLogDir = sboxLogDirConfig
	}

	if sboxLogDir == "" {
		return "", errors.Errorf("container %s has a sandbox with an empty log path", sboxLogDir)
	}

	logPath := c.config.GetLogPath()
	if logPath == "" {
		logPath = filepath.Join(sboxLogDir, c.ID()+".log")
	} else {
		logPath = filepath.Join(sboxLogDir, logPath)
	}

	// Handle https://issues.k8s.io/44043
	if err := utils.EnsureSaneLogPath(logPath); err != nil {
		return "", err
	}

	logrus.Debugf("setting container's log_path = %s, sbox.logdir = %s, ctr.logfile = %s",
		sboxLogDir, c.config.GetLogPath(), logPath,
	)
	return logPath, nil
}

// DisableFips returns whether the container should disable fips mode
func (c *container) DisableFips() bool {
	if value, ok := c.sboxConfig.GetLabels()["FIPS_DISABLE"]; ok && value == "true" {
		return true
	}
	return false
}

// Image returns the image specified in the container spec, or an error
func (c *container) Image() (string, error) {
	imageSpec := c.config.GetImage()
	if imageSpec == nil {
		return "", errors.New("CreateContainerRequest.ContainerConfig.Image is nil")
	}

	image := imageSpec.Image
	if image == "" {
		return "", errors.New("CreateContainerRequest.ContainerConfig.Image.Image is empty")
	}
	return image, nil
}

// ReadOnly returns whether the rootfs should be readonly
// it takes a bool as to whether crio was configured to
// be readonly, which it defaults to if the container wasn't
// specifically asked to be read only
func (c *container) ReadOnly(serverIsReadOnly bool) bool {
	if c.config.GetLinux().GetSecurityContext().GetReadonlyRootfs() {
		return true
	}
	return serverIsReadOnly
}

// SelinuxLabel returns the container's SelinuxLabel
// it takes the sandbox's label, which it falls back upon
func (c *container) SelinuxLabel(sboxLabel string) ([]string, error) {
	selinuxConfig := c.config.GetLinux().GetSecurityContext().GetSelinuxOptions()

	labels := map[string]string{}

	labelOptions, err := label.DupSecOpt(sboxLabel)
	if err != nil {
		return nil, err
	}
	for _, r := range labelOptions {
		k := strings.Split(r, ":")[0]
		labels[k] = r
	}

	if selinuxConfig != nil {
		for _, r := range utils.GetLabelOptions(selinuxConfig) {
			k := strings.Split(r, ":")[0]
			labels[k] = r
		}
	}
	ret := []string{}
	for _, v := range labels {
		ret = append(ret, v)
	}
	return ret, nil
}

// InitSpecGen initializes a new config spec generator.
func (c *container) InitSpecGen() error {
	specGen, err := generate.New("linux")
	if err != nil {
		return errors.Wrap(err, "creating new linux spec")
	}
	c.specGen = &specGen
	return nil
}

// SpecGen returns the internal config spec generator.
func (c *container) SpecGen() *generate.Generator {
	return c.specGen
}

// ApplyAppArmor takes the AppArmor config and applies it to the internal
// config spec generator.
func (c *container) ApplyAppArmor(appArmorConfig *apparmor.Config) error {
	if c.specGen == nil {
		return errors.New("config spec generator of container is not set")
	}
	if appArmorConfig.IsEnabled() && !c.Privileged() {
		profile, err := appArmorConfig.Apply(
			c.config.GetLinux().GetSecurityContext().GetApparmorProfile(),
		)
		if err != nil {
			return errors.Wrapf(
				err, "applying apparmor profile to container %s", c.ID(),
			)
		}

		log.Debugf(c.ctx,
			"Applied AppArmor profile %s to container %s", profile, c.ID(),
		)
		c.SpecGen().SetProcessApparmorProfile(profile)
	}

	return nil
}
