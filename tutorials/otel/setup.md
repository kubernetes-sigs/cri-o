# Getting started with otel tracing on Kubernetes (Centos-8)

## Setting up the VM

### Before you begin
* A compatible Linux host. The Kubernetes project provides generic instructions for Linux distributions based on Debian and Red Hat, and those distributions without a package manager.
* 2 GB or more of RAM per machine (any less will leave little room for your apps).
* 2 CPUs or more.
* Full network connectivity between all machines in the cluster (public or private network is fine).
* Swap disabled. You MUST disable swap.


### Installing dependecies

```sh
sudo dnf install -y wget nano
```

```sh
sudo dnf install -y \
  containers-common \
  git \
  glib2-devel \
  glibc-devel \
  libgpg-error-devel \
  libseccomp-devel \
  libselinux-devel \
  pkgconfig \
  make \
  runc
```

```sh
sudo dnf --enablerepo=powertools install -y device-mapper-devel \
    glibc-static \
    gpgme-devel \
    libassuan-devel
```

```sh
sudo dnf groupinstall "Development Tools"
```


### Install GO

Get the latest installer from https://golang.org/doc/install

```sh
cd $HOME
wget <replace with latest go installer>
tar -xzf go1*tar.gz && rm -rf go1*tar.gz
sudo cp -r $HOME/go /usr/local/
export PATH=$PATH:/usr/local/go/bin # or add to PATH in ~/.bashrc
go version
```
```sh
go get github.com/cpuguy83/go-md2man
```


### Installing `btrfs-progs-devel`

```sh
# Download latest elrepo-release rpm from http://mirror.rackspace.com/elrepo/elrepo/el8/x86_64/RPMS/

# Install elrepo-release rpm:
sudo rpm -Uvh elrepo-release*rpm

#Install btrfs-progs-devel rpm package:
sudo dnf --enablerepo=elrepo-testing install -y btrfs-progs-devel golang-github-cpuguy83-go-md2man
```

## Get `cri-o` Source Code and build from source

### Clone the source code using:

```sh
git clone https://github.com/cri-o/cri-o # or your fork
```

### Building without seccomp support 

```sh
cd cri-o
make BUILDTAGS=""
sudo make install
```

### Download conmon
`conmon` is a per-container daemon that CRI-O uses to monitor container logs and exit information. conmon needs to be downloaded with CRI-O.

```sh
git clone https://github.com/containers/conmon
cd conmon
make
sudo make install #will download conmon to your $PATH.
```

### Setup CNI networking

```sh
sudo wget https://raw.githubusercontent.com/cri-o/cri-o/master/contrib/cni/11-crio-ipv4-bridge.conf -P /etc/cni/net.d
```

### Installing CNI plugins from source

In addition, you need to install the CNI plugins necessary into `/opt/cni/bin` (or the directories specified by crio.network.plugin_dir). The two plugins necessary for the example CNI configurations are loopback and bridge. Download and set up the CNI plugins by following the below steps:


```sh
git clone https://github.com/containernetworking/plugins
cd plugins
git checkout v0.8.7

./build_linux.sh # or build_windows.sh

Output:

Building plugins
  bandwidth
  firewall
  flannel
  portmap
  sbr
  tuning
  bridge
  host-device
  ipvlan
  loopback
  macvlan
  ptp
  vlan
  dhcp
  host-local
  static

sudo mkdir -p /opt/cni/bin
sudo cp bin/* /opt/cni/bin/
```

### CRI-O configuration
If you are installing for the first time, generate and install configuration files with:

```sh 
cd cri-o
sudo make install.config
```
### Validate registries in registries.conf
Edit `/etc/containers/registries.conf` and verify that the registries option has valid values in it. For example:
```sh
[registries.search]
registries = ['registry.access.redhat.com', 'registry.fedoraproject.org', 'quay.io', 'docker.io']

[registries.insecure]
registries = []

[registries.block]
registries = []
```

### Optional - Modify verbosity of logs
Users can modify the log_level by specifying an overwrite like `/etc/crio/crio.conf.d/01-log-level.conf` to change the verbosity of the logs. Options are fatal, panic, error, warn, info (default), debug and trace.
```sh
[crio.runtime]
log_level = "debug"
```

### Starting CRI-O

Running make install will download CRI-O into `/usr/local/bin/crio`

You can run it manually there, or you can set up a systemd unit file with:

```sh
sudo make install.systemd

# And let systemd take care of running CRI-O:

sudo systemctl daemon-reload
sudo systemctl enable crio
sudo systemctl start crio
```

## Kubeadm install process

