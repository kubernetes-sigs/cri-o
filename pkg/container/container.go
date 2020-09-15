package container

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman/v3/pkg/annotations"
	"github.com/containers/storage/pkg/stringid"
	"github.com/cri-o/cri-o/internal/config/device"
	"github.com/cri-o/cri-o/internal/lib"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/storage"
	crioann "github.com/cri-o/cri-o/pkg/annotations"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/cri-o/cri-o/utils"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	kubeletTypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// Container is the main public container interface
type Container interface {
	// All set methods are usually called in order of their definition

	// SetConfig sets the configuration to the container and validates it
	SetConfig(*types.ContainerConfig, *types.PodSandboxConfig) error

	// SetNameAndID sets a container name and ID
	// It can either generate a new ID or use an existing ID
	// if specified as parameter (for container restore)
	SetNameAndID(string) error

	// Config returns the container CRI configuration
	Config() *types.ContainerConfig

	// SandboxConfig returns the sandbox CRI configuration
	SandboxConfig() *types.PodSandboxConfig

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

	// SetRestore marks the container as being restored from a checkpoint
	SetRestore(bool)

	// Restore returns if the container is marked as being
	// restored from a checkpoint
	Restore() bool

	// spec functions

	// returns the spec
	Spec() *generate.Generator

	// SpecAddMount adds a mount to the container's spec
	// it takes the rspec mount object
	// if there is already a mount at the path specified, it removes it.
	SpecAddMount(rspec.Mount)

	// SpecAddAnnotations adds annotations to the spec.
	SpecAddAnnotations(ctx context.Context, sandbox *sandbox.Sandbox, containerVolume []oci.ContainerVolume, mountPoint, configStopSignal string, imageResult *storage.ImageResult, isSystemd, systemdHasCollectMode bool) error

	// SpecAddDevices adds devices from the server config, and container CRI config
	SpecAddDevices([]device.Device, []device.Device, bool) error

	// AddUnifiedResourcesFromAnnotations adds the cgroup-v2 resources specified in the io.kubernetes.cri-o.UnifiedCgroup annotation
	AddUnifiedResourcesFromAnnotations(annotationsMap map[string]string) error

	// SpecSetProcessArgs sets the process args in the spec,
	// given the image information and passed-in container config
	SpecSetProcessArgs(imageOCIConfig *v1.Image) error

	// WillRunSystemd checks whether the process args
	// are configured to be run as a systemd instance.
	WillRunSystemd() bool
}

// container is the hidden default type behind the Container interface
type container struct {
	config     *types.ContainerConfig
	sboxConfig *types.PodSandboxConfig
	id         string
	name       string
	privileged bool
	restore    bool
	spec       generate.Generator
}

// New creates a new, empty Sandbox instance
func New() (Container, error) {
	spec, err := generate.New("linux")
	if err != nil {
		return nil, err
	}
	return &container{
		spec: spec,
	}, nil
}

// SpecAddMount adds a specified mount to the spec
func (c *container) SpecAddMount(r rspec.Mount) {
	c.spec.RemoveMount(r.Destination)
	c.spec.AddMount(r)
}

// SpecAddAnnotation adds all annotations to the spec
func (c *container) SpecAddAnnotations(ctx context.Context, sb *sandbox.Sandbox, containerVolumes []oci.ContainerVolume, mountPoint, configStopSignal string, imageResult *storage.ImageResult, isSystemd, systemdHasCollectMode bool) (err error) {
	// Copied from k8s.io/kubernetes/pkg/kubelet/kuberuntime/labels.go
	const podTerminationGracePeriodLabel = "io.kubernetes.pod.terminationGracePeriod"

	kubeAnnotations := c.Config().Annotations
	created := time.Now()
	labels := c.Config().Labels

	image, err := c.Image()
	if err != nil {
		return err
	}
	logPath, err := c.LogPath(sb.LogDir())
	if err != nil {
		return err
	}

	// Preserve the sandbox annotations. OCI hooks may re-use the sandbox
	// annotation values to apply them to the container later on.
	// The sandbox annotations are already filtered for the allowed
	// annotations, there is no need to check it additionally here.
	for k, v := range sb.Annotations() {
		if strings.HasPrefix(k, crioann.OCISeccompBPFHookAnnotation) {
			// The OCI seccomp BPF hook
			// (https://github.com/containers/oci-seccomp-bpf-hook)
			// uses the annotation io.containers.trace-syscall as indicator
			// to attach a BFP module to the process. The recorded syscalls
			// will be then stored in the output path file (annotation
			// value prefixed with 'of:'). We now add a custom logic to be
			// able to distinguish containers within pods in Kubernetes. If
			// we suffix the container name within the annotation key like
			// this: io.containers.trace-syscall/container
			// Then we will rewrite the key to
			// 'io.containers.trace-syscall' if the metadata name is equal
			// to 'container'. This allows us to trace containers into
			// distinguishable files.
			if strings.TrimPrefix(k, crioann.OCISeccompBPFHookAnnotation+"/") == c.config.Metadata.Name {
				log.Debugf(ctx,
					"Annotation key for container %q rewritten to %q (value is: %q)",
					c.config.Metadata.Name, crioann.OCISeccompBPFHookAnnotation, v,
				)
				c.config.Annotations[crioann.OCISeccompBPFHookAnnotation] = v
				c.spec.AddAnnotation(crioann.OCISeccompBPFHookAnnotation, v)
			} else {
				// Annotation not suffixed with the container name
				c.spec.AddAnnotation(k, v)
			}
		}
	}

	c.spec.AddAnnotation(annotations.Image, image)
	c.spec.AddAnnotation(annotations.ImageName, imageResult.Name)
	c.spec.AddAnnotation(annotations.ImageRef, imageResult.ID)
	c.spec.AddAnnotation(annotations.Name, c.Name())
	c.spec.AddAnnotation(annotations.ContainerID, c.ID())
	c.spec.AddAnnotation(annotations.SandboxID, sb.ID())
	c.spec.AddAnnotation(annotations.SandboxName, sb.Name())
	c.spec.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeContainer)
	c.spec.AddAnnotation(annotations.LogPath, logPath)
	c.spec.AddAnnotation(annotations.TTY, strconv.FormatBool(c.Config().Tty))
	c.spec.AddAnnotation(annotations.Stdin, strconv.FormatBool(c.Config().Stdin))
	c.spec.AddAnnotation(annotations.StdinOnce, strconv.FormatBool(c.Config().StdinOnce))
	c.spec.AddAnnotation(annotations.ResolvPath, sb.ResolvPath())
	c.spec.AddAnnotation(annotations.ContainerManager, lib.ContainerManagerCRIO)
	c.spec.AddAnnotation(annotations.MountPoint, mountPoint)
	c.spec.AddAnnotation(annotations.SeccompProfilePath, c.Config().Linux.SecurityContext.SeccompProfilePath)
	c.spec.AddAnnotation(annotations.Created, created.Format(time.RFC3339Nano))

	metadataJSON, err := json.Marshal(c.Config().Metadata)
	if err != nil {
		return err
	}
	c.spec.AddAnnotation(annotations.Metadata, string(metadataJSON))

	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	c.spec.AddAnnotation(annotations.Labels, string(labelsJSON))

	volumesJSON, err := json.Marshal(containerVolumes)
	if err != nil {
		return err
	}
	c.spec.AddAnnotation(annotations.Volumes, string(volumesJSON))

	kubeAnnotationsJSON, err := json.Marshal(kubeAnnotations)
	if err != nil {
		return err
	}
	c.spec.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))

	for k, v := range kubeAnnotations {
		c.spec.AddAnnotation(k, v)
	}
	for k, v := range labels {
		c.spec.AddAnnotation(k, v)
	}
	for idx, ip := range sb.IPs() {
		c.spec.AddAnnotation(fmt.Sprintf("%s.%d", annotations.IP, idx), ip)
	}

	if isSystemd {
		if t, ok := kubeAnnotations[podTerminationGracePeriodLabel]; ok {
			// currently only supported by systemd, see
			// https://github.com/opencontainers/runc/pull/2224
			c.spec.AddAnnotation("org.systemd.property.TimeoutStopUSec", "uint64 "+t+"000000") // sec to usec
		}
		if systemdHasCollectMode {
			c.spec.AddAnnotation("org.systemd.property.CollectMode", "'inactive-or-failed'")
		}
		c.spec.AddAnnotation("org.systemd.property.DefaultDependencies", "true")
		c.spec.AddAnnotation("org.systemd.property.After", "['crio.service']")
	}

	if configStopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		c.spec.AddAnnotation("org.opencontainers.image.stopSignal", configStopSignal)
	}

	return nil
}

func (c *container) Spec() *generate.Generator {
	return &c.spec
}

// SetConfig sets the configuration to the container and validates it
func (c *container) SetConfig(config *types.ContainerConfig, sboxConfig *types.PodSandboxConfig) error {
	if c.config != nil {
		return errors.New("config already set")
	}

	if config == nil {
		return errors.New("config is nil")
	}

	if config.Metadata == nil {
		return errors.New("metadata is nil")
	}

	if config.Metadata.Name == "" {
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
func (c *container) SetNameAndID(oldID string) error {
	if c.config == nil {
		return errors.New("config is not set")
	}

	if c.sboxConfig == nil {
		return errors.New("sandbox config is nil")
	}

	if c.sboxConfig.Metadata == nil {
		return errors.New("sandbox metadata is nil")
	}

	var id string
	if oldID == "" {
		id = stringid.GenerateNonCryptoID()
	} else {
		id = oldID
	}
	name := strings.Join([]string{
		"k8s",
		c.config.Metadata.Name,
		c.sboxConfig.Metadata.Name,
		c.sboxConfig.Metadata.Namespace,
		c.sboxConfig.Metadata.UID,
		fmt.Sprintf("%d", c.config.Metadata.Attempt),
	}, "_")

	c.id = id
	c.name = name
	return nil
}

// Config returns the container configuration
func (c *container) Config() *types.ContainerConfig {
	return c.config
}

// SandboxConfig returns the sandbox configuration
func (c *container) SandboxConfig() *types.PodSandboxConfig {
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

// Restore returns if the container is marked as being
// restored from a checkpoint
func (c *container) Restore() bool {
	return c.restore
}

// SetRestore marks the container as being restored from a checkpoint
func (c *container) SetRestore(restore bool) {
	c.restore = restore
}

// SetPrivileged sets the privileged bool for the container
func (c *container) SetPrivileged() error {
	if c.config == nil {
		return nil
	}
	if c.config.Linux == nil {
		return nil
	}
	if c.config.Linux.SecurityContext == nil {
		return nil
	}

	if c.sboxConfig == nil {
		return nil
	}

	if c.sboxConfig.Linux == nil {
		return nil
	}

	if c.sboxConfig.Linux.SecurityContext == nil {
		return nil
	}

	if c.config.Linux.SecurityContext.Privileged {
		if !c.sboxConfig.Linux.SecurityContext.Privileged {
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
	sboxLogDirConfig := c.sboxConfig.LogDirectory
	if sboxLogDirConfig != "" {
		sboxLogDir = sboxLogDirConfig
	}

	if sboxLogDir == "" {
		return "", errors.Errorf("container %s has a sandbox with an empty log path", sboxLogDir)
	}

	logPath := c.config.LogPath
	if logPath == "" {
		logPath = filepath.Join(sboxLogDir, c.ID()+".log")
	} else {
		logPath = filepath.Join(sboxLogDir, logPath)
	}

	// Handle https://issues.k8s.io/44043
	if err := utils.EnsureSaneLogPath(logPath); err != nil {
		return "", err
	}

	logrus.Debugf("Setting container's log_path = %s, sbox.logdir = %s, ctr.logfile = %s",
		sboxLogDir, c.config.LogPath, logPath,
	)
	return logPath, nil
}

// DisableFips returns whether the container should disable fips mode
func (c *container) DisableFips() bool {
	if value, ok := c.sboxConfig.Labels["FIPS_DISABLE"]; ok && value == "true" {
		return true
	}
	return false
}

// Image returns the image specified in the container spec, or an error
func (c *container) Image() (string, error) {
	imageSpec := c.config.Image
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
	if c.config.Linux.SecurityContext.ReadonlyRootfs {
		return true
	}
	return serverIsReadOnly
}

// SelinuxLabel returns the container's SelinuxLabel
// it takes the sandbox's label, which it falls back upon
func (c *container) SelinuxLabel(sboxLabel string) ([]string, error) {
	selinuxConfig := c.config.Linux.SecurityContext.SelinuxOptions

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

// AddUnifiedResourcesFromAnnotations adds the cgroup-v2 resources specified in the io.kubernetes.cri-o.UnifiedCgroup annotation
func (c *container) AddUnifiedResourcesFromAnnotations(annotationsMap map[string]string) error {
	if c.config == nil || c.config.Labels == nil {
		return nil
	}
	containerName := c.config.Labels[kubeletTypes.KubernetesContainerNameLabel]
	if containerName == "" {
		return nil
	}

	annotationKey := fmt.Sprintf("%s.%s", crioann.UnifiedCgroupAnnotation, containerName)
	annotation := annotationsMap[annotationKey]
	if annotation == "" {
		return nil
	}

	if c.spec.Config.Linux == nil {
		c.spec.Config.Linux = &rspec.Linux{}
	}
	if c.spec.Config.Linux.Resources == nil {
		c.spec.Config.Linux.Resources = &rspec.LinuxResources{}
	}
	if c.spec.Config.Linux.Resources.Unified == nil {
		c.spec.Config.Linux.Resources.Unified = make(map[string]string)
	}
	for _, r := range strings.Split(annotation, ";") {
		parts := strings.SplitN(r, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid annotation %q", crioann.UnifiedCgroupAnnotation)
		}
		d, err := b64.StdEncoding.DecodeString(parts[1])
		// if the value is not specified in base64, then use its raw value.
		v := ""
		if err == nil {
			v = string(d)
		} else {
			v = parts[1]
		}
		c.spec.Config.Linux.Resources.Unified[parts[0]] = v
	}

	return nil
}

// SpecSetProcessArgs sets the process args in the spec,
// given the image information and passed-in container config
func (c *container) SpecSetProcessArgs(imageOCIConfig *v1.Image) error {
	kubeCommands := c.config.Command
	kubeArgs := c.config.Args

	// merge image config and kube config
	// same as docker does today...
	if imageOCIConfig != nil {
		if len(kubeCommands) == 0 {
			if len(kubeArgs) == 0 {
				kubeArgs = imageOCIConfig.Config.Cmd
			}
			if kubeCommands == nil {
				kubeCommands = imageOCIConfig.Config.Entrypoint
			}
		}
	}

	// create entrypoint and args
	var entrypoint string
	var args []string
	switch {
	case len(kubeCommands) != 0:
		entrypoint = kubeCommands[0]
		args = kubeCommands[1:]
		args = append(args, kubeArgs...)
	case len(kubeArgs) != 0:
		entrypoint = kubeArgs[0]
		args = kubeArgs[1:]
	default:
		return errors.New("no command specified")
	}

	c.spec.SetProcessArgs(append([]string{entrypoint}, args...))
	return nil
}

// WillRunSystemd checks whether the process args
// are configured to be run as a systemd instance.
func (c *container) WillRunSystemd() bool {
	entrypoint := c.spec.Config.Process.Args[0]
	return strings.Contains(entrypoint, "/sbin/init") || (filepath.Base(entrypoint) == "systemd")
}
