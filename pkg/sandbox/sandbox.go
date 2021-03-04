package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containers/libpod/v2/pkg/annotations"
	"github.com/containers/storage/pkg/stringid"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/hostport"
	"github.com/cri-o/cri-o/internal/lib"
	ann "github.com/cri-o/cri-o/pkg/annotations"
	"github.com/cri-o/cri-o/pkg/container"
	"github.com/cri-o/cri-o/server/cri/types"
	json "github.com/json-iterator/go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	kubeletTypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// Sandbox is the interface for managing pod sandboxes
type Sandbox interface {
	Create() error

	Start() error

	Stop() error

	Delete() error

	AddContainer(container.Container) error

	RemoveContainer(container.Container) error

	// SetConfig sets the sandbox configuration and validates it
	SetConfig(*types.PodSandboxConfig) error

	// SetNameAndID sets the sandbox name and ID
	SetNameAndID() error

	// Config returns the sandbox configuration
	Config() *types.PodSandboxConfig

	// ID returns the id of the pod sandbox
	ID() string

	// Name returns the id of the pod sandbox
	Name() string

	// returns the spec
	Spec() *generate.Generator

	SpecAddAnnotations(string, string, string, string, string, string, string, string, string, string, string, string, string, []string, bool, bool) error
}

// sandbox is the hidden default type behind the Sandbox interface
type sandbox struct {
	ctx    context.Context
	config *types.PodSandboxConfig
	id     string
	name   string
	spec   generate.Generator
}

// New creates a new, empty Sandbox instance
func New(ctx context.Context) (Sandbox, error) {
	spec, err := generate.New("linux")
	if err != nil {
		return nil, err
	}

	return &sandbox{
		ctx:    ctx,
		config: nil,
		spec:   spec,
	}, nil
}

// SetConfig sets the sandbox configuration and validates it
func (s *sandbox) SetConfig(config *types.PodSandboxConfig) error {
	if s.config != nil {
		return errors.New("config already set")
	}

	if config == nil {
		return errors.New("config is nil")
	}

	if config.Metadata == nil {
		return errors.New("metadata is nil")
	}

	if config.Metadata.Name == "" {
		return errors.New("PodSandboxConfig.Metadata.Name should not be empty")
	}
	s.config = config
	return nil
}

// returns the spec
func (s *sandbox) Spec() *generate.Generator {
	return &s.spec
}

func (s *sandbox) SpecAddAnnotations(pauseImage, containerName, shmPath, privileged, runtimeHandler, resolvPath, hostname, stopSignal, cgroupParent, mountPoint, hostnamePath, cniResultJSON, created string, ips []string, isSystemd, spoofedContainer bool) (err error) {
	kubeAnnotations := s.config.Annotations
	// add metadata
	metadata := s.config.Metadata
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	labels := s.config.Labels

	// Add special container name label for the infra container
	if s.config.Labels != nil {
		labels[kubeletTypes.KubernetesContainerNameLabel] = leaky.PodInfraContainerName
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return err
	}

	// add annotations
	kubeAnnotationsJSON, err := json.Marshal(s.config.Annotations)
	if err != nil {
		return err
	}

	securityContext := s.config.Linux.SecurityContext
	nsOptsJSON, err := json.Marshal(securityContext.NamespaceOptions)
	if err != nil {
		return err
	}

	logDir := s.config.LogDirectory
	logPath := filepath.Join(logDir, s.ID()+".log")
	kubeName := s.config.Metadata.Name

	hostNetwork := securityContext.NamespaceOptions.Network == types.NamespaceModeNODE

	s.spec.AddAnnotation(annotations.Metadata, string(metadataJSON))
	s.spec.AddAnnotation(annotations.Labels, string(labelsJSON))
	s.spec.AddAnnotation(annotations.Annotations, string(kubeAnnotationsJSON))
	s.spec.AddAnnotation(annotations.LogPath, logPath)
	s.spec.AddAnnotation(annotations.Name, s.Name())
	s.spec.AddAnnotation(annotations.Namespace, s.config.Metadata.Namespace)
	s.spec.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	s.spec.AddAnnotation(annotations.SandboxID, s.ID())
	s.spec.AddAnnotation(annotations.Image, pauseImage)
	s.spec.AddAnnotation(annotations.ContainerName, containerName)
	s.spec.AddAnnotation(annotations.ContainerID, s.ID())
	s.spec.AddAnnotation(annotations.ShmPath, shmPath)
	s.spec.AddAnnotation(annotations.PrivilegedRuntime, fmt.Sprintf("%v", privileged))
	s.spec.AddAnnotation(annotations.RuntimeHandler, runtimeHandler)
	s.spec.AddAnnotation(annotations.ResolvPath, resolvPath)
	s.spec.AddAnnotation(annotations.HostName, hostname)
	s.spec.AddAnnotation(annotations.NamespaceOptions, string(nsOptsJSON))
	s.spec.AddAnnotation(annotations.KubeName, kubeName)
	s.spec.AddAnnotation(annotations.HostNetwork, fmt.Sprintf("%v", hostNetwork))
	s.spec.AddAnnotation(annotations.ContainerManager, lib.ContainerManagerCRIO)
	s.spec.AddAnnotation(annotations.Created, created)
	s.spec.AddAnnotation(annotations.CgroupParent, cgroupParent)
	s.spec.AddAnnotation(annotations.MountPoint, mountPoint)
	s.spec.AddAnnotation(annotations.HostnamePath, hostnamePath)

	if stopSignal != "" {
		// this key is defined in image-spec conversion document at https://github.com/opencontainers/image-spec/pull/492/files#diff-8aafbe2c3690162540381b8cdb157112R57
		s.spec.AddAnnotation("org.opencontainers.image.stopSignal", stopSignal)
	}
	if isSystemd && node.SystemdHasCollectMode() {
		s.spec.AddAnnotation("org.systemd.property.CollectMode", "'inactive-or-failed'")
	}

	portMappings := convertPortMappings(s.Config().PortMappings)
	portMappingsJSON, err := json.Marshal(portMappings)
	if err != nil {
		return err
	}
	s.spec.AddAnnotation(annotations.PortMappings, string(portMappingsJSON))

	for k, v := range kubeAnnotations {
		s.spec.AddAnnotation(k, v)
	}
	for k, v := range labels {
		s.spec.AddAnnotation(k, v)
	}

	if spoofedContainer {
		s.spec.AddAnnotation(ann.SpoofedContainer, "true")
	}

	if cniResultJSON != "" {
		s.spec.AddAnnotation(annotations.CNIResult, cniResultJSON)
	}

	for idx, ip := range ips {
		s.spec.AddAnnotation(fmt.Sprintf("%s.%d", annotations.IP, idx), ip)
	}
	return nil
}

// SetNameAndID sets the sandbox name and ID
func (s *sandbox) SetNameAndID() error {
	if s.config == nil {
		return errors.New("config is nil")
	}

	if s.config.Metadata.Namespace == "" {
		return errors.New("cannot generate pod name without namespace")
	}

	if s.config.Metadata.Name == "" {
		return errors.New("cannot generate pod name without name in metadata")
	}

	s.id = stringid.GenerateNonCryptoID()
	s.name = strings.Join([]string{
		"k8s",
		s.config.Metadata.Name,
		s.config.Metadata.Namespace,
		s.config.Metadata.UID,
		fmt.Sprintf("%d", s.config.Metadata.Attempt),
	}, "_")

	return nil
}

func convertPortMappings(in []*types.PortMapping) []*hostport.PortMapping {
	out := make([]*hostport.PortMapping, 0, len(in))
	for _, v := range in {
		if v.HostPort <= 0 {
			continue
		}
		out = append(out, &hostport.PortMapping{
			HostPort:      v.HostPort,
			ContainerPort: v.ContainerPort,
			Protocol:      v1.Protocol(v.Protocol.String()),
			HostIP:        v.HostIP,
		})
	}
	return out
}

// Config returns the sandbox configuration
func (s *sandbox) Config() *types.PodSandboxConfig {
	return s.config
}

// ID returns the id of the pod sandbox
func (s *sandbox) ID() string {
	return s.id
}

// Name returns the id of the pod sandbox
func (s *sandbox) Name() string {
	return s.name
}
