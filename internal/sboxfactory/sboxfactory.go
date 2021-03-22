package sboxfactory

import (
	"fmt"
	"strings"

	"github.com/containers/storage/pkg/stringid"
	ctrfactory "github.com/cri-o/cri-o/internal/ctrfactory"
	"github.com/cri-o/cri-o/server/cri/types"
	"github.com/pkg/errors"
)

// SandboxFactory is a structure for creating a new Sandbox
type SandboxFactory struct {
	config     *types.PodSandboxConfig
	id         string
	name       string
	infra      *ctrfactory.ContainerFactory
	resolvPath string
}

// New creates a new, empty Sandbox instance
func New() *SandboxFactory {
	return &SandboxFactory{
		config: nil,
	}
}

// SetConfig sets the sandbox configuration and validates it
func (s *SandboxFactory) SetConfig(config *types.PodSandboxConfig) error {
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

// SetNameAndID sets the sandbox name and ID
func (s *SandboxFactory) SetNameAndID() error {
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

// Config returns the sandbox configuration
func (s *SandboxFactory) Config() *types.PodSandboxConfig {
	return s.config
}

// ID returns the id of the pod sandbox
func (s *SandboxFactory) ID() string {
	return s.id
}

// Name returns the id of the pod sandbox
func (s *SandboxFactory) Name() string {
	return s.name
}

// ResolvPath returns the sandbox's resolvPath
func (s *SandboxFactory) ResolvPath() string {
	return s.resolvPath
}
