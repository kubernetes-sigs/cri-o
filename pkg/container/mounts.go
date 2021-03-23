package container

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containers/buildah/pkg/secrets"
	"github.com/containers/libpod/v2/pkg/rootless"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/mount"
	"github.com/containers/storage/pkg/stringid"
	"github.com/cri-o/cri-o/internal/config/node"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	oci "github.com/cri-o/cri-o/internal/oci"
	"github.com/cri-o/cri-o/internal/storage"
	sconfig "github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server/cri/types"
	securejoin "github.com/cyphar/filepath-securejoin"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

func (c *container) SetupMounts(ctx context.Context, serverConfig *sconfig.Config, sb *sandbox.Sandbox, containerInfo storage.ContainerInfo, mountPoint string) ([]oci.ContainerVolume, []rspec.Mount, error) {
	// To start, make sure each container gets /sys/fs/cgroup mounted
	c.AddMount(&rspec.Mount{
		Destination: "/sys/fs/cgroup",
		Type:        "cgroup",
		Source:      "cgroup",
		Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
	})

	// Then, add the special mounts for systemd.
	// TODO FIXME duplicated condition
	if c.WillRunSystemd() {
		c.AddMountsForSystemd()
	}

	// Next, add mounts for a readOnly container
	c.addReadOnlyMounts(serverConfig.ReadOnly)

	if sb.HostNetwork() {
		c.AddMount(&rspec.Mount{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "ro"},
		})
	}

	if c.Privileged() {
		c.AddMount(&rspec.Mount{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "rw", "rslave"},
		})
		c.AddMount(&rspec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "rw", "relatime", "rslave"},
		})
	}

	c.AddMount(&rspec.Mount{
		Destination: "/dev/shm",
		Type:        "bind",
		Source:      sb.ShmPath(),
		Options:     []string{"rw", "bind"},
	})

	options := []string{"rw"}
	// TODO FIXME duplicated check
	if c.ReadOnly(serverConfig.ReadOnly) {
		options = []string{"ro"}
	}
	if sb.ResolvPath() != "" {
		if err := SecurityLabel(sb.ResolvPath(), containerInfo.MountLabel, false); err != nil {
			return nil, nil, err
		}
		c.AddMount(&rspec.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      sb.ResolvPath(),
			Options:     append(options, []string{"bind", "nodev", "nosuid", "noexec"}...),
		})
	}

	if sb.HostnamePath() != "" {
		if err := SecurityLabel(sb.HostnamePath(), containerInfo.MountLabel, false); err != nil {
			return nil, nil, err
		}
		c.AddMount(&rspec.Mount{
			Destination: "/etc/hostname",
			Type:        "bind",
			Source:      sb.HostnamePath(),
			Options:     append(options, "bind"),
		})
	}

	if hostNetwork(c.config) {
		// Only bind mount for host netns and when CRI does not give us any hosts file
		c.AddMount(&rspec.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      "/etc/hosts",
			Options:     append(options, "bind"),
		})
	}

	if c.Privileged() {
		setOCIBindMountsPrivileged(c.Spec())
	}

	// Add image volumes
	err := c.addImageVolumes(ctx, mountPoint, serverConfig, &containerInfo)
	if err != nil {
		return nil, nil, err
	}

	containerVolumes, err := c.addOCIBindMounts(ctx, containerInfo.MountLabel, serverConfig.RuntimeConfig.BindMountPrefix)
	if err != nil {
		return nil, nil, err
	}

	// Add secrets from the default and override mounts.conf files
	secretMounts := secrets.SecretMounts(containerInfo.MountLabel, containerInfo.RunDir, serverConfig.DefaultMountsFile, rootless.IsRootless(), c.DisableFips())
	for _, m := range secretMounts {
		c.AddMount(&rspec.Mount{
			Type:        "bind",
			Options:     append(m.Options, "bind"),
			Destination: m.Destination,
			Source:      m.Source,
		})
	}

	c.SpecAddMounts()

	return containerVolumes, secretMounts, nil
}

func (c *container) SpecAddMounts() {
	allMounts := make([]*rspec.Mount, len(c.mounts))

	// filter out /dev and /sys
	devSet, sysSet := false, false
	for dest, m := range c.mounts {
		if dest == "/dev" {
			devSet = true
		}
		if dest == "/sys" {
			sysSet = true
		}
		allMounts = append(allMounts, m)
	}

	sort.Sort(orderedMounts(allMounts))

	for _, m := range allMounts {
		if devSet && strings.HasPrefix(m.Destination, "/dev/") {
			continue
		}
		if sysSet && strings.HasPrefix(m.Destination, "/sys/") {
			continue
		}
		c.spec.RemoveMount(m.Destination)
		c.spec.AddMount(*m)
	}
}

func (c *container) addReadOnlyMounts(serverIsReadOnly bool) {
	if !serverIsReadOnly {
		return
	}
	// tmpcopyup is a runc extension and is not part of the OCI spec.
	// WORK ON: Use "overlay" mounts as an alternative to tmpfs with tmpcopyup
	// Look at https://github.com/cri-o/cri-o/pull/1434#discussion_r177200245 for more info on this
	options := []string{"rw", "noexec", "nosuid", "nodev", "tmpcopyup"}
	mounts := map[string]string{
		"/run":     "mode=0755",
		"/tmp":     "mode=1777",
		"/var/tmp": "mode=1777",
	}
	for target, mode := range mounts {
		c.AddMount(&rspec.Mount{
			Destination: target,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     append(options, mode),
		})
	}
}

func setOCIBindMountsPrivileged(g *generate.Generator) {
	spec := g.Config
	// clear readonly for /sys and cgroup
	for i := range spec.Mounts {
		clearReadOnly(&spec.Mounts[i])
	}
	spec.Linux.ReadonlyPaths = nil
	spec.Linux.MaskedPaths = nil
}

// systemd expects to have /run, /run/lock and /tmp on tmpfs
// It also expects to be able to write to /sys/fs/cgroup/systemd and /var/log/journal
func (c *container) AddMountsForSystemd() {
	options := []string{"rw", "rprivate", "noexec", "nosuid", "nodev"}
	for _, dest := range []string{"/run", "/run/lock"} {
		c.AddMount(&rspec.Mount{
			Destination: dest,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     append(options, "tmpcopyup"),
		})
	}
	for _, dest := range []string{"/tmp", "/var/log/journal"} {
		c.AddMount(&rspec.Mount{
			Destination: dest,
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     append(options, "tmpcopyup"),
		})
	}

	if node.CgroupIsV2() {
		c.AddMount(&rspec.Mount{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"private", "rw"},
		})
	} else {
		c.AddMount(&rspec.Mount{
			Destination: "/sys/fs/cgroup/systemd",
			Type:        "bind",
			Source:      "/sys/fs/cgroup/systemd",
			Options:     []string{"bind", "nodev", "noexec", "nosuid"},
		})
		c.spec.AddLinuxMaskedPaths("/sys/fs/cgroup/systemd/release_agent")
	}
	c.spec.AddProcessEnv("container", "crio")
}

func (c *container) addMount(m *rspec.Mount) {
	if m == nil {
		return
	}
	c.mounts[m.Destination] = m
}

func (c *container) addImageVolumes(ctx context.Context, rootfs string, serverConfig *sconfig.Config, containerInfo *storage.ContainerInfo) error {
	for dest := range containerInfo.Config.Config.Volumes {
		fp, err := securejoin.SecureJoin(rootfs, dest)
		if err != nil {
			return err
		}
		switch serverConfig.ImageVolumes {
		case sconfig.ImageVolumesMkdir:
			IDs := idtools.IDPair{UID: int(c.Spec().Config.Process.User.UID), GID: int(c.Spec().Config.Process.User.GID)}
			if err1 := idtools.MkdirAllAndChownNew(fp, 0o755, IDs); err1 != nil {
				return err1
			}
			if containerInfo.MountLabel != "" {
				if err1 := SecurityLabel(fp, containerInfo.MountLabel, true); err1 != nil {
					return err1
				}
			}
		case sconfig.ImageVolumesBind:
			volumeDirName := stringid.GenerateNonCryptoID()
			src := filepath.Join(containerInfo.RunDir, "mounts", volumeDirName)
			if err1 := os.MkdirAll(src, 0o755); err1 != nil {
				return err1
			}
			// Label the source with the sandbox selinux mount label
			if containerInfo.MountLabel != "" {
				if err1 := SecurityLabel(src, containerInfo.MountLabel, true); err1 != nil {
					return err1
				}
			}

			log.Debugf(ctx, "Adding bind mounted volume: %s to %s", src, dest)
			c.AddMount(&rspec.Mount{
				Source:      src,
				Destination: dest,
				Type:        "bind",
				Options:     []string{"private", "bind", "rw"},
			})

		case sconfig.ImageVolumesIgnore:
			log.Debugf(ctx, "ignoring volume %v", dest)
		default:
			log.Errorf(ctx, "unrecognized image volumes setting")
		}
	}
	return nil
}

func (c *container) addOCIBindMounts(ctx context.Context, mountLabel string, bindMountPrefix string) ([]oci.ContainerVolume, error) {
	volumes := []oci.ContainerVolume{}
	mounts := c.config.Mounts

	// Sort mounts in number of parts. This ensures that high level mounts don't
	// shadow other mounts.
	sort.Sort(criOrderedMounts(mounts))

	defaultMounts := c.spec.Mounts()
	c.spec.ClearMounts()
	for _, m := range defaultMounts {
		// We will override with specified mounts below,
		// and clean up /dev and /sys (if specified) when adding them
		// to the spec generator.
		c.AddMount(&m)
	}

	mountInfos, err := mount.GetMounts()
	if err != nil {
		return nil, err
	}
	for _, m := range mounts {
		dest := m.ContainerPath
		if dest == "" {
			return nil, fmt.Errorf("mount.ContainerPath is empty")
		}

		if m.HostPath == "" {
			return nil, fmt.Errorf("mount.HostPath is empty")
		}
		src := filepath.Join(bindMountPrefix, m.HostPath)

		resolvedSrc, err := resolveSymbolicLink(bindMountPrefix, src)
		if err == nil {
			src = resolvedSrc
		} else {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to resolve symlink %q: %v", src, err)
			} else if err = os.MkdirAll(src, 0o755); err != nil {
				return nil, fmt.Errorf("failed to mkdir %s: %s", src, err)
			}
		}

		options := []string{"rw"}
		if m.Readonly {
			options = []string{"ro"}
		}
		options = append(options, "rbind")

		// mount propagation
		switch m.Propagation {
		case types.MountPropagationPropagationPrivate:
			options = append(options, "rprivate")
			// Since default root propagation in runc is rprivate ignore
			// setting the root propagation
		case types.MountPropagationPropagationBidirectional:
			if err := ensureShared(src, mountInfos); err != nil {
				return nil, err
			}
			options = append(options, "rshared")
			if err := c.spec.SetLinuxRootPropagation("rshared"); err != nil {
				return nil, err
			}
		case types.MountPropagationPropagationHostToContainer:
			if err := ensureSharedOrSlave(src, mountInfos); err != nil {
				return nil, err
			}
			options = append(options, "rslave")
			if c.spec.Config.Linux.RootfsPropagation != "rshared" &&
				c.spec.Config.Linux.RootfsPropagation != "rslave" {
				if err := c.spec.SetLinuxRootPropagation("rslave"); err != nil {
					return nil, err
				}
			}
		default:
			log.Warnf(ctx, "unknown propagation mode for hostPath %q", m.HostPath)
			options = append(options, "rprivate")
		}

		if m.SelinuxRelabel {
			if err := SecurityLabel(src, mountLabel, false); err != nil {
				return nil, err
			}
		}

		volumes = append(volumes, oci.ContainerVolume{
			ContainerPath: dest,
			HostPath:      src,
			Readonly:      m.Readonly,
		})

		c.AddMount(&rspec.Mount{
			Source:      src,
			Destination: dest,
			Options:     options,
		})
	}

	return volumes, nil
}

func SecurityLabel(path, secLabel string, shared bool) error {
	if err := label.Relabel(path, secLabel, shared); err != nil && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("relabel failed %s: %v", path, err)
	}
	return nil
}

func hostNetwork(containerConfig *types.ContainerConfig) bool {
	securityContext := containerConfig.Linux.SecurityContext
	if securityContext == nil || securityContext.NamespaceOptions == nil {
		return false
	}

	return securityContext.NamespaceOptions.Network == types.NamespaceModeNODE
}

type orderedMounts []*rspec.Mount

// Len returns the number of mounts. Used in sorting.
func (m orderedMounts) Len() int {
	return len(m)
}

// Less returns true if the number of parts (a/b/c would be 3 parts) in the
// mount indexed by parameter 1 is less than that of the mount indexed by
// parameter 2. Used in sorting.
func (m orderedMounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

// Swap swaps two items in an array of mounts. Used in sorting
func (m orderedMounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

// parts returns the number of parts in the destination of a mount. Used in sorting.
func (m orderedMounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].Destination), string(os.PathSeparator))
}

// mounts defines how to sort runtime.Mount.
// This is the same with the Docker implementation:
//   https://github.com/moby/moby/blob/17.05.x/daemon/volumes.go#L26
type criOrderedMounts []*types.Mount

// Len returns the number of mounts. Used in sorting.
func (m criOrderedMounts) Len() int {
	return len(m)
}

// Less returns true if the number of parts (a/b/c would be 3 parts) in the
// mount indexed by parameter 1 is less than that of the mount indexed by
// parameter 2. Used in sorting.
func (m criOrderedMounts) Less(i, j int) bool {
	return m.parts(i) < m.parts(j)
}

// Swap swaps two items in an array of mounts. Used in sorting
func (m criOrderedMounts) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

// parts returns the number of parts in the destination of a mount. Used in sorting.
func (m criOrderedMounts) parts(i int) int {
	return strings.Count(filepath.Clean(m[i].ContainerPath), string(os.PathSeparator))
}

// resolveSymbolicLink resolves a possible symlink path. If the path is a symlink, returns resolved
// path; if not, returns the original path.
// note: strictly SecureJoin is not sufficient, as it does not error when a part of the path doesn't exist
// but simply moves on. If the last part of the path doesn't exist, it may need to be created.
func resolveSymbolicLink(scope, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != os.ModeSymlink {
		return path, nil
	}
	if scope == "" {
		scope = "/"
	}
	return securejoin.SecureJoin(scope, path)
}

// Ensure mount point on which path is mounted, is shared.
func ensureShared(path string, mountInfos []*mount.Info) error {
	sourceMount, optionalOpts, err := getSourceMount(path, mountInfos)
	if err != nil {
		return err
	}

	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		}
	}

	return fmt.Errorf("path %q is mounted on %q but it is not a shared mount", path, sourceMount)
}

// Ensure mount point on which path is mounted, is either shared or slave.
func ensureSharedOrSlave(path string, mountInfos []*mount.Info) error {
	sourceMount, optionalOpts, err := getSourceMount(path, mountInfos)
	if err != nil {
		return err
	}
	// Make sure source mount point is shared.
	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			return nil
		} else if strings.HasPrefix(opt, "master:") {
			return nil
		}
	}
	return fmt.Errorf("path %q is mounted on %q but it is not a shared or slave mount", path, sourceMount)
}

func clearReadOnly(m *rspec.Mount) {
	var opt []string
	for _, o := range m.Options {
		if o == "rw" {
			return
		} else if o != "ro" {
			opt = append(opt, o)
		}
	}
	m.Options = opt
	m.Options = append(m.Options, "rw")
}

func getSourceMount(source string, mountinfos []*mount.Info) (path, optional string, _ error) {
	var res *mount.Info

	for _, mi := range mountinfos {
		// check if mi can be a parent of source
		if strings.HasPrefix(source, mi.Mountpoint) {
			// look for a longest one
			if res == nil || len(mi.Mountpoint) > len(res.Mountpoint) {
				res = mi
			}
		}
	}
	if res == nil {
		return "", "", fmt.Errorf("could not find source mount of %s", source)
	}

	return res.Mountpoint, res.Optional, nil
}

func (c *container) AddMount(m *rspec.Mount) {
	if m == nil {
		return
	}
	c.mounts[filepath.Clean(m.Destination)] = m
}
