![CRI-O logo](https://github.com/cri-o/cri-o/blob/master/logo/crio-logo.svg?raw=true)
# CRI-O Installation Instructions

This guide will walk you through the installation of [CRI-O](https://github.com/cri-o/cri-o), an Open Container Initiative-based implementation of the [Kubernetes Container Runtime Interface](https://github.com/kubernetes/community/blob/master/contributors/design-proposals/node/container-runtime-interface-v1.md).
It is assumed you are running a Linux machine.

**Table of Contents**:
- [Install packaged versions of CRI-O](#install-packaged-versions-of-cri-o)
  * [Supported versions](#supported-versions)
  * [Installation Instructions](#installation-instructions)
    + [openSUSE](#openSUSE)
    + [Fedora 31 or later](#fedora-31-or-later)
    + [Other yum based operating systems](#other-yum-based-operating-systems)
    + [APT based operating systems](#apt-based-operating-systems)
- [Build and install CRI-O from source](#build-and-install-cri-o-from-source)
  * [Runtime dependencies](#runtime-dependencies)
  * [Build and Run Dependencies](#build-and-run-dependencies)
    + [Fedora - RHEL 7 - CentOS](#fedora---rhel-7---centos)
    + [RHEL 8](#rhel-8)
    + [Debian - Raspbian - Ubuntu](#debian---raspbian---ubuntu)
  * [Get Source Code](#get-source-code)
  * [Build](#build)
    + [Install with Ansible](#install-with-ansible)
    + [Build Tags](#build-tags)
  * [Static builds](#static-builds)
    + [Creating a release archive](#creating-a-release-archive)
  * [Download conmon](#download-conmon)
  * [Setup CNI networking](#setup-cni-networking)
  * [CRI-O configuration](#cri-o-configuration)
    + [Validate registries in registries.conf](#validate-registries-in-registriesconf)
    + [Recommended - Use systemd cgroups.](#recommended---use-systemd-cgroups)
    + [Optional - Modify verbosity of logs](#optional---modify-verbosity-of-logs)
    + [Optional - Modify capabilities and sysctls](#optional---modify-capabilities-and-sysctls)
  * [Starting CRI-O](#starting-cri-o)
  * [Using CRI-O](#using-cri-o)

## Install packaged versions of CRI-O

CRI-O builds for native package managers using [openSUSE's OBS](build.opensuse.org)

### Supported versions

CRI-O follows the [Kubernetes support cycle](https://kubernetes.io/docs/setup/release/version-skew-policy/#supported-versions) of three minor releases.
CRI-O also attempts to package for the following operating systems:
```
Fedora 31+
openSUSE
CentOS 8
CentOS 8 Stream
CentOS 7
Debian Unstable
Debian Testing
Debian 10
Rasbian 10
xUbuntu 20.10
xUbuntu 20.04
xUbuntu 18.04
```

To install, choose a supported version for your operating system, and export it as a variable, like so:
`export VERSION=1.19`

We also save releases as subprojects. If you'd, for instance, like to use `1.18.3` you can set
`export VERSION=1.19:1.19.1`

Packaging for CRI-O is done best-effort, and is largely driven by requests.
If there's a version or operating system that is missing, please [open an issue](github.com/cri-o/cri-o/issues/new).

### Installation Instructions

#### openSUSE:

```shell
sudo zypper install cri-o
```

#### Fedora 31 or later

```shell
sudo dnf module enable cri-o:$VERSION
sudo dnf install cri-o
```

For Fedora, we only support setting minor versions. i.e: `VERSION=1.18`, and do not support pinning patch versions: `VERSION=1.18.3`

#### Other yum based operating systems

To install on the following operating systems, set the environment variable $OS as the appropriate field in the following table:

| Operating system | $OS               |
| ---------------- | ----------------- |
| Centos 8         | `CentOS_8`        |
| Centos 8 Stream  | `CentOS_8_Stream` |
| Centos 7         | `CentOS_7`        |

And then run the following as root:

```shell
curl -L -o /etc/yum.repos.d/devel:kubic:libcontainers:stable.repo https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/$OS/devel:kubic:libcontainers:stable.repo
curl -L -o /etc/yum.repos.d/devel:kubic:libcontainers:stable:cri-o:$VERSION.repo https://download.opensuse.org/repositories/devel:kubic:libcontainers:stable:cri-o:$VERSION/$OS/devel:kubic:libcontainers:stable:cri-o:$VERSION.repo
yum install cri-o
```

#### APT based operating systems

Note: this tutorial assumes you have curl and gnupg installed

To install on the following operating systems, set the environment variable $OS as the appropriate field in the following table:
| Operating system | $OS               |
| ---------------- | ----------------- |
| Debian Unstable  | `Debian_Unstable` |
| Debian Testing   | `Debian_Testing`  |
| Raspberry Pi OS  | `Raspbian_10`     |
| Ubuntu 20.04     | `xUbuntu_20.04`   |
| Ubuntu 19.10     | `xUbuntu_19.10`   |
| Ubuntu 19.04     | `xUbuntu_19.04`   |
| Ubuntu 18.04     | `xUbuntu_18.04`   |

If installing cri-o-runc (recommended), you'll need to install libseccomp >= 2.4.1. This is not available in distros based on Debian buster or below, so buster backports will need to be enabled:

```shell
echo 'deb http://deb.debian.org/debian buster-backports main' > /etc/apt/sources.list.d/backports.list
apt update
apt install -y libseccomp2 || apt update -y libseccomp2
```

And then run the following as root:

```shell
echo "deb https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/$OS/ /" > /etc/apt/sources.list.d/devel:kubic:libcontainers:stable.list
echo "deb https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable:/cri-o:/$VERSION/$OS/ /" > /etc/apt/sources.list.d/devel:kubic:libcontainers:stable:cri-o:$VERSION.list

curl -L https://download.opensuse.org/repositories/devel:kubic:libcontainers:stable:cri-o:$VERSION/$OS/Release.key | apt-key add -
curl -L https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable/$OS/Release.key | apt-key add -

apt-get update
apt-get install cri-o cri-o-runc
```

Note: We include cri-o-runc because Ubuntu and Debian include their own packaged version of runc.
While this version should work with CRI-O, keeping the packaged versions of CRI-O and runc in sync ensures they work together.
If you'd like to use the distribution's runc, you'll have to add the file:

```toml
[crio.runtime.runtimes.runc]
runtime_path = ""
runtime_type = "oci"
runtime_root = "/run/runc"
```

to `/etc/crio/crio.conf.d/`

## Build and install CRI-O from source

### Runtime dependencies

- runc, Clear Containers runtime, or any other OCI compatible runtime
- iproute
- iptables

Latest version of `runc` is expected to be installed on the system. It is picked up as the default runtime by CRI-O.

### Build and Run Dependencies

#### Fedora - RHEL 7 - CentOS

**Required**

Fedora, RHEL 7, CentOS and related distributions:

```bash
yum install -y \
  containers-common \
  device-mapper-devel \
  git \
  glib2-devel \
  glibc-devel \
  glibc-static \
  go \
  gpgme-devel \
  libassuan-devel \
  libgpg-error-devel \
  libseccomp-devel \
  libselinux-devel \
  pkgconfig \
  make \
  runc
```

**Please note**:

- `CentOS 8` (or higher): `pkgconfig` package is replaced by `pkgconf-pkg-config`
- By default btrfs is not enabled. To add the btrfs support, install the
  following package: `btrfs-progs-devel`
- It is possible the distribution packaged version of runc is out of date. If you'd like to get the latest and greatest runc, consider using the one found in https://build.opensuse.org/project/show/devel:kubic:libcontainers:stable

#### RHEL 8

RHEL 8 distributions:\
Make sure you are subscribed to the following repositories: \
BaseOS/x86_64 \
Appstream/x86_64
CodeReady Linux Builder for x86_64

```
subscription-manager repos --enable=rhel-8-for-x86_64-baseos-rpms
subscription-manager repos --enable=rhel-8-for-x86_64-appstream-rpms
subscription-manager repos --enable=codeready-builder-for-rhel-8-x86_64-rpms
```

Follow the guide below to subscribe to the repositories if not already subscribed:\
https://access.redhat.com/solutions/265523

This requires go version 1.12 or greater:
```
yum module -y install go-toolset
```

```bash
yum install -y \
  containers-common \
  device-mapper-devel \
  git \
  make \
  glib2-devel \
  glibc-devel \
  glibc-static \
  runc \
```

Here is a link on how to install a source rpm on RHEL: \
https://www.itechlounge.net/2012/12/linux-how-to-install-source-rpm-on-rhelcentos/

Dependency: gpgme-devel \
Link: http://download.eng.bos.redhat.com/brewroot/packages/gpgme/1.10.0/6.el8/x86_64/

Dependency: go-md2man \
Command:
```
go get github.com/cpuguy83/go-md2man
```

The following dependencies:
```bash
  libassuan \
  libassuan-devel \
  libgpg-error \
  libseccomp \
  libselinux \
  pkgconf-pkg-config \
```

#### Debian - Raspbian - Ubuntu
On Debian, Raspbian and Ubuntu distributions, [enable the Kubic project
repositories](../README.md#installing-crio) and install the following packages:

```bash
apt-get update -qq && apt-get install -y \
  btrfs-tools \
  containers-common \
  git \
  golang-go \
  libassuan-dev \
  libdevmapper-dev \
  libglib2.0-dev \
  libc6-dev \
  libgpgme11-dev \
  libgpg-error-dev \
  libseccomp-dev \
  libsystemd-dev \
  libselinux1-dev \
  pkg-config \
  go-md2man \
  cri-o-runc \
  libudev-dev \
  software-properties-common \
  gcc \
  make
```

**Caveats and Notes:**

If using an older release or a long-term support release, be careful to double-check that the version of `runc` is new enough (running `runc --version` should produce `spec: 1.0.0`), or else build your own.

Be careful to double-check that the version of golang is new enough, version
1.12.x or higher is required. If needed, newer golang versions are available at
[the official download website](https://golang.org/dl).

### Get Source Code

Clone the source code using:

```bash
git clone https://github.com/cri-o/cri-o # or your fork
cd cri-o
```

Make sure your `CRI-O` and `kubernetes` versions are of matching major versions.
For instance, if you want to be compatible with the latest kubernetes release,
you'll need to use the latest tagged release of `CRI-O` on branch `release-1.18`.

### Build

To install with default buildtags using seccomp, use:

```bash
make
sudo make install
```

Otherwise, if you do not want to build `CRI-O` with seccomp support you can add `BUILDTAGS=""` when running make.

```bash
make BUILDTAGS=""
sudo make install
```

#### Install with Ansible

An [Ansible Role](https://github.com/alvistack/ansible-role-cri_o) is also available to automate the above steps:

``` bash
sudo su -
mkdir -p ~/.ansible/roles
cd ~/.ansible/roles
git clone https://github.com/alvistack/ansible-role-cri_o.git cri_o
cd ~/.ansible/roles/cri_o
pip3 install --upgrade --ignore-installed --requirement requirements.txt
molecule converge
molecule verify
```

#### Build Tags

`CRI-O` supports optional build tags for compiling support of various features.
To add build tags to the make option the `BUILDTAGS` variable must be set.

```bash
make BUILDTAGS='seccomp apparmor'
```

| Build Tag                        | Feature                                         | Dependency   |
|----------------------------------|-------------------------------------------------|--------------|
| seccomp                          | syscall filtering                               | libseccomp   |
| selinux                          | selinux process and mount labeling              | libselinux   |
| apparmor                         | apparmor profile support                        | <none>       |

`CRI-O` manages images with [containers/image](https://github.com/containers/image), which uses the following buildtags.

| Build Tag                        | Feature                                         | Dependency   |
|----------------------------------|-------------------------------------------------|--------------|
| containers_image_openpgp         | use native golang pgp instead of cgo            | <none>       |
| containers_image_ostree_stub     | disable use of ostree as an image transport     | <none>       |

`CRI-O` also uses [containers/storage](https://github.com/containers/storage) for managing container storage.

| Build Tag                        | Feature                                         | Dependency   |
|----------------------------------|-------------------------------------------------|--------------|
| exclude_graphdriver_btrfs        | exclude btrfs as a storage option               | <none>       |
| btrfs_noversion                  | for building btrfs version < 3.16.1             | btrfs        |
| exclude_graphdriver_devicemapper | exclude devicemapper as a storage option        | <none>       |
| libdm_no_deferred_remove         | don't compile deferred remove with devicemapper | devicemapper |
| exclude_graphdriver_overlay      | exclude overlay as a storage option             | <none>       |
| ostree                           | build storage using ostree                      | ostree       |

### Static builds

It is possible to build a statically linked binary of CRI-O by using the
officially provided [nix](https://nixos.org/nix) package and the derivation of
it [within this repository](../nix). The builds are completely reproducible and
will create a `x86_64`/`amd64` or `aarch64`/`arm64` stripped ELF binary for
[glibc](https://www.gnu.org/software/libc). These binaries are integration
tested as well and support the following features:

- apparmor
- btrfs
- device mapper
- gpgme
- seccomp
- selinux

To build the binaries locally either [install the nix package
manager](https://nixos.org/nix/download.html) or use the `make build-static`
target which relies on the nixos/nix container image.

The overall build process can take a tremendous amount of CPU time depending on
the hardware. The resulting binaries should now be available within:

- `bin/static/crio`

To build the binaries without any prepared container and via the already
installed nix package manager, simply run the following command from the root
directory of this repository:

```
nix build -f nix
```

The resulting binaries should be now available in `result/bin`. To build the arm
variant of the binaries, just run:

```
nix build -f nix/default-arm64.nix
```

#### Creating a release archive

A release bundle consists of all static binaries, the man pages and
configuration files like `00-default.conf`. The `release-bundle` target can be
used to build a new release archive within the current repository:

```
make release-bundle
…
Created ./bundle/cri-o.amd64.v1.20.0.tar.gz
```

### Download conmon
[conmon](https://github.com/containers/conmon) is a per-container daemon that `CRI-O` uses to monitor container logs and exit information.
`conmon` needs to be downloaded with `CRI-O`.

running:
```bash
git clone https://github.com/containers/conmon
cd conmon
make
sudo make install
```
will download conmon to your $PATH.


### Setup CNI networking

A proper description of setting up CNI networking is given in the
[`contrib/cni` README](/contrib/cni/README.md). But the gist is that you need to
have some basic network configurations enabled and CNI plugins installed on
your system.

### CRI-O configuration

If you are installing for the first time, generate and install configuration files with:

```
sudo make install.config
```

### Validate registries in registries.conf

Edit `/etc/containers/registries.conf` and verify that the registries option has valid values in it.  For example:

```
[registries.search]
registries = ['registry.access.redhat.com', 'registry.fedoraproject.org', 'quay.io', 'docker.io']

[registries.insecure]
registries = []

[registries.block]
registries = []
```

For more information about this file see [registries.conf(5)](https://github.com/containers/image/blob/master/docs/containers-registries.conf.5.md).


#### Optional - Modify verbosity of logs

Users can modify the `log_level` by specifying an overwrite like
`/etc/crio/crio.conf.d/01-log-level.conf` to change the verbosity of
the logs. Options are fatal, panic, error, warn, info (default), debug and
trace.

```
[crio.runtime]
log_level = "info"
```

#### Optional - Modify capabilities and sysctls
By default, `CRI-O` uses the following capabilities:

```
default_capabilities = [
	"CHOWN",
	"DAC_OVERRIDE",
	"FSETID",
	"FOWNER",
	"SETGID",
	"SETUID",
	"SETPCAP",
	"NET_BIND_SERVICE",
	"KILL",
]
```
and no sysctls
```
default_sysctls = [
]
```

Users can change either default by adding overwrites to `/etc/crio/crio.conf.d`.

### Starting CRI-O

Running make install will download CRI-O into the folder
```bash
/usr/local/bin/crio
```

You can run it manually there, or you can set up a systemd unit file with:

```
sudo make install.systemd
```

And let systemd take care of running CRI-O:

``` bash
sudo systemctl daemon-reload
sudo systemctl enable crio
sudo systemctl start crio
```

### Using CRI-O

- Follow this [tutorial](tutorials/crictl.md) to quickly get started running simple pods and containers.
- To run a full cluster, see [the instructions](tutorials/kubernetes.md).
- To run with kubeadm, see [kubeadm instructions](tutorials/kubeadm.md).

### Updating CRI-O

#### openSUSE:
```
sudo zypper update
sudo zypper update cri-o
```

### Fedora 31 or later
```
sudo dnf update
sudo dnf update cri-o
```

### Other yum based operating systems

```
sudo yum update
sudo yum update cri-o
```

### APT based operating systems

If updating to a patch version (for example, ``VERSION=1.8.3``
  ), run
```
apt update cri-o cri-o-runc
```
Otherwise, be sure that the environment variable ```$OS``` is set to your operating system as the appropriate field in the following table
To install on the following operating systems, set the environment variable $OS as the appropriate field in the following table:

| Operating system | $OS               |
| ---------------- | ----------------- |
| Debian Unstable  | `Debian_Unstable` |
| Debian Testing   | `Debian_Testing`  |
| Ubuntu 20.04     | `xUbuntu_20.04`   |
| Ubuntu 19.10     | `xUbuntu_19.10`   |
| Ubuntu 19.04     | `xUbuntu_19.04`   |
| Ubuntu 18.04     | `xUbuntu_18.04`   |

To upgrade, choose a supported version for your operating system, and export it as a variable, like so:
`export VERSION=1.18`, and run the following as root
```
rm /etc/apt/sources.list.d/devel:kubic:libcontainers:stable:cri-o:$VERSION.list

echo "deb http://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/stable:/cri-o:/$VERSION/$OS/ /" > /etc/apt/sources.list.d/devel:kubic:libcontainers:stable:cri-o:$VERSION.list

curl -L https://download.opensuse.org/repositories/devel:kubic:libcontainers:stable:cri-o:$VERSION/$OS/Release.key | apt-key add -

apt update
apt install cri-o cri-o-runc

```
