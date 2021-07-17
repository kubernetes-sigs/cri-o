#!/bin/sh -ex

# package installer for *cri-o* container runtime <https://cri-o.io>
sudo -v

# systemd distro
. /etc/os-release

# cri-o version
export VERSION=${1:-1.20}

case "${ID}-${VERSION_ID}" in
fedora-*)
    OS="Fedora_${VERSION_ID}"
    PKG=yum
    ;;
centos-*)
    OS="CentOS_${VERSION_ID}"
    PKG=yum
    ;;
ubuntu-*)
    OS="xUbuntu_${VERSION_ID}"
    PKG=apt
    ;;
debian-*)
    OS="Debian_${VERSION_ID}"
    PKG=apt
    ;;
*)
    echo "ERROR: Unsupported distribution '${PRETTY_NAME}'" >&2
    exit 1
    ;;
esac

# <https://build.opensuse.org/project/show/devel:kubic:libcontainers>

URL="https://download.opensuse.org/repositories"
REPO="$URL/devel:kubic:libcontainers"
SLASH="$URL/devel:/kubic:/libcontainers"
FILE="devel:kubic:libcontainers"
NAME="cri-o"
CHANNEL="stable"

if [ "$PKG" = "yum" ]; then
    curl -L "$SLASH:/$CHANNEL/$OS/$FILE:$CHANNEL.repo" | sudo tee /etc/yum.repos.d/"$FILE:$CHANNEL".repo
    curl -L "$REPO:$CHANNEL:$NAME:$VERSION/$OS/$FILE:$CHANNEL:$NAME:$VERSION.repo" | sudo tee /etc/yum.repos.d/"$FILE:$CHANNEL:$NAME:$VERSION".repo

    sudo yum install -y cri-o
fi

if [ "$PKG" = "apt" ]; then
    echo "deb $SLASH:/$CHANNEL/$OS/ /" | sudo tee /etc/apt/sources.list.d/"$FILE:$CHANNEL".list
    echo "deb $SLASH:/$CHANNEL:/$NAME:/$VERSION/$OS/ /" | sudo tee /etc/apt/sources.list.d/"$FILE:$CHANNEL:$NAME:$VERSION".list

    curl -L "$REPO:$CHANNEL:$NAME:$VERSION/$OS/Release.key" | sudo apt-key add -
    curl -L "$SLASH:/$CHANNEL/$OS/Release.key" | sudo apt-key add -

    sudo apt-get update
    sudo apt-get install -y cri-o cri-o-runc
fi
