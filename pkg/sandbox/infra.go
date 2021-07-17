package sandbox

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cri-o/cri-o/internal/storage"
	libconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/pkg/container"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const (
	// According to http://man7.org/linux/man-pages/man5/resolv.conf.5.html:
	// "The search list is currently limited to six domains with a total of 256 characters."
	maxDNSSearches = 6
)

func (s *sandbox) InitInfraContainer(serverConfig *libconfig.Config, podContainer *storage.ContainerInfo) error {
	var err error
	s.infra, err = container.New()
	if err != nil {
		return err
	}

	// determine pause command before initializing generator
	// so a failure will not result in a half configured generator
	pauseCommand, err := PauseCommand(serverConfig, podContainer.Config)
	if err != nil {
		return err
	}

	g := s.infra.Spec()
	g.HostSpecific = true

	// setup defaults for the pod sandbox
	g.SetRootReadonly(true)

	// configure default ulimits
	for _, u := range serverConfig.Ulimits() {
		g.AddProcessRlimits(u.Name, u.Hard, u.Soft)
	}
	g.SetProcessArgs(pauseCommand)

	if err := s.createResolvConf(podContainer); err != nil {
		return errors.Wrapf(err, "create resolv conf")
	}

	return nil
}

// Spec can only be called after a successful call to InitInfraContainer
func (s *sandbox) Spec() *generate.Generator {
	return s.infra.Spec()
}

// PauseCommand returns the pause command for the provided image configuration.
func PauseCommand(cfg *libconfig.Config, image *v1.Image) ([]string, error) {
	if cfg == nil {
		return nil, errors.Errorf("provided configuration is nil")
	}

	// This has been explicitly set by the user, since the configuration
	// default is `/pause`
	if cfg.PauseCommand != "" {
		return []string{cfg.PauseCommand}, nil
	}
	if image == nil || (len(image.Config.Entrypoint) == 0 && len(image.Config.Cmd) == 0) {
		return nil, fmt.Errorf(
			"unable to run pause image %q: %s",
			cfg.PauseImage,
			"neither Cmd nor Entrypoint specified",
		)
	}
	cmd := []string{}
	cmd = append(cmd, image.Config.Entrypoint...)
	cmd = append(cmd, image.Config.Cmd...)
	return cmd, nil
}

func (s *sandbox) createResolvConf(podContainer *storage.ContainerInfo) (retErr error) {
	// set DNS options
	if s.config.DNSConfig == nil {
		return nil
	}

	dnsServers := s.config.DNSConfig.Servers
	dnsSearches := s.config.DNSConfig.Searches
	dnsOptions := s.config.DNSConfig.Options
	s.resolvPath = fmt.Sprintf("%s/resolv.conf", podContainer.RunDir)
	err := ParseDNSOptions(dnsServers, dnsSearches, dnsOptions, s.resolvPath)
	defer func() {
		if retErr != nil {
			if err := os.Remove(s.resolvPath); err != nil {
				retErr = errors.Wrapf(retErr, "failed to remove resolvPath after failing to create it")
			}
		}
	}()
	if err != nil {
		return err
	}

	if err := label.Relabel(s.resolvPath, podContainer.MountLabel, false); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	mnt := spec.Mount{
		Type:        "bind",
		Source:      s.resolvPath,
		Destination: "/etc/resolv.conf",
		Options:     []string{"ro", "bind", "nodev", "nosuid", "noexec"},
	}
	s.infra.Spec().AddMount(mnt)
	return nil
}

func ParseDNSOptions(servers, searches, options []string, path string) (retErr error) {
	nServers := len(servers)
	nSearches := len(searches)
	nOptions := len(options)
	if nServers == 0 && nSearches == 0 && nOptions == 0 {
		return copyFile("/etc/resolv.conf", path)
	}

	if nSearches > maxDNSSearches {
		return fmt.Errorf("DNSOption.Searches has more than %d domains", maxDNSSearches)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if nSearches > 0 {
		_, err = f.WriteString("search " + strings.Join(searches, " ") + "\n")
		if err != nil {
			return err
		}
	}

	if nServers > 0 {
		_, err = f.WriteString("nameserver " + strings.Join(servers, "\nnameserver ") + "\n")
		if err != nil {
			return err
		}
	}

	if nOptions > 0 {
		_, err = f.WriteString("options " + strings.Join(options, " ") + "\n")
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
