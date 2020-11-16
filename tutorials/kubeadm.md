# Running CRI-O with kubeadm

This tutorial assumes you've already installed and setup CRI-O. If you have not, start [here](install-distro.md).
It also assumes you've set up your system to use kubeadm. If you haven't done so, [here is a good tutorial](https://www.mirantis.com/blog/how-install-kubernetes-kubeadm/)

### Configuring CNI

You'll need to use your plugins to figure out your pod-network-cidr. If you use the default bridge plugin defined [here](/contrib/cni/10-crio-bridge.conf), set
```CIDR=10.85.0.0/16```
If you're using a flannel network, set
```CIDR=10.244.0.0/16```

# Configuring kubelet

There is a handy, pre-prepared `/etc/default/kubelet` file [here](https://gist.githubusercontent.com/haircommander/2c07cc23887fa7c7f083dc61c7ef5791/raw/73e3d27dcd57e7de237c08758f76e0a368547648/cri-o-kubeadm)
This will configure kubeadm to run with the correct defaults CRI-O needs to run.

Note: This file assumes you've set your cgroup_driver as systemd

# Running kubeadm

Given you've set CIDR, and you've properly set the kubelet file, all you need to do is start crio (as defined [here](setup.md)), and run:
`kubeadm init --pod-network-cidr=$CIDR`

# Running kubeadm in an off line network

We will assume that the user has installed CRI-O and all necessary packages. We will also assume that all necessary components are configured and everything is working as expected. The user should have a private repo where the container images are pushed. An example of container images for Kubernetes version 1.18.2:

```bash
$ kubeadm config images list --image-repository user.private.repo --kubernetes-version=v1.18.2
user.private.repo/kube-apiserver:v1.18.2
user.private.repo/kube-controller-manager:v1.18.2
user.private.repo/kube-scheduler:v1.18.2
user.private.repo/kube-proxy:v1.18.2
user.private.repo/pause:3.2
user.private.repo/etcd:3.4.3-0
user.private.repo/coredns:1.6.7
```

The user needs to configure the [registries.conf](https://github.com/containers/image/blob/master/docs/containers-registries.conf.5.md) file.

Sample of configurations:

```bash
$ cat /etc/containers/registries.conf
unqualified-search-registries = ["user.private.repo"]

[[registry]]
prefix = "k8s.gcr.io"
insecure = false
blocked = false
location = "k8s.gcr.io"

[[registry.mirror]]
location = "user.private.repo"
```

Next the user should reload and restart the CRI-O service to load the configurations.

The user needs to add the cgroup driver to the configuration file or pass it as a parameter when starting kubeadm for the master node. The following are samples of parameters for the configuration file:

```bash
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
```
