go 1.14

module github.com/cri-o/cri-o

require (
	github.com/BurntSushi/toml v0.3.1
	github.com/Microsoft/go-winio v0.4.15-0.20200113171025-3fe6c5262873
	github.com/blang/semver v3.5.1+incompatible
	github.com/containerd/containerd v1.3.3
	github.com/containerd/release-tool v0.0.0-20200402162031-a35b5d7ce53d
	github.com/containerd/ttrpc v0.0.0-20200121165050-0be804eadb15
	github.com/containernetworking/cni v0.7.2-0.20200304161608-4fae32b84921
	github.com/containernetworking/plugins v0.8.5
	github.com/containers/buildah v1.14.8
	github.com/containers/common v0.9.1
	github.com/containers/conmon v2.0.15+incompatible
	github.com/containers/image/v5 v5.5.1
	github.com/containers/libpod v1.9.2
	github.com/containers/ocicrypt v1.0.2
	github.com/containers/storage v1.20.2
	github.com/coreos/go-systemd/v22 v22.0.0
	github.com/cpuguy83/go-md2man v1.0.10
	github.com/creack/pty v1.1.11
	github.com/cri-o/ocicni v0.2.1-0.20200422173648-513ef787b8c9
	github.com/cyphar/filepath-securejoin v0.2.2
	github.com/docker/distribution v2.7.1+incompatible
	github.com/docker/docker v1.4.2-0.20191219165747-a9416c67da9f // indirect
	github.com/docker/go-units v0.4.0
	github.com/fsnotify/fsnotify v1.4.9
	github.com/go-zoo/bone v1.3.0
	github.com/godbus/dbus/v5 v5.0.3
	github.com/gogo/protobuf v1.3.1
	github.com/golang/mock v1.4.3
	github.com/google/renameio v0.1.0
	github.com/google/uuid v1.1.1
	github.com/grpc-ecosystem/go-grpc-middleware v1.2.0
	github.com/hpcloud/tail v1.0.0
	github.com/onsi/ginkgo v1.12.0
	github.com/onsi/gomega v1.9.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.2-0.20200206005212-79b036d80240
	github.com/opencontainers/runc v1.0.0-rc90
	github.com/opencontainers/runtime-spec v1.0.2
	github.com/opencontainers/runtime-tools v0.9.1-0.20200121211434-d1bf3e66ff0a
	github.com/opencontainers/selinux v1.5.2
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.5.1
	github.com/psampaz/go-mod-outdated v0.6.0
	github.com/seccomp/containers-golang v0.3.2
	github.com/sirupsen/logrus v1.6.0
	github.com/soheilhy/cmux v0.1.4
	github.com/syndtr/gocapability v0.0.0-20180916011248-d98352740cb2
	github.com/urfave/cli/v2 v2.2.0
	github.com/vbatts/git-validation v1.1.0
	github.com/vishvananda/netlink v1.1.0
	golang.org/x/net v0.0.0-20200324143707-d3edc9973b7e
	golang.org/x/sync v0.0.0-20200317015054-43a5402ce75a
	golang.org/x/sys v0.0.0-20200519105757-fe76b779f299
	google.golang.org/grpc v1.28.1
	k8s.io/api v0.17.4
	k8s.io/apimachinery v0.17.4
	k8s.io/client-go v0.0.0
	k8s.io/cri-api v0.0.0
	k8s.io/klog v1.0.0
	k8s.io/kubernetes v1.18.1
	k8s.io/release v0.2.5
	k8s.io/utils v0.0.0-20200327001022-6496210b90e8
	mvdan.cc/sh/v3 v3.1.0
)

replace (
	github.com/godbus/dbus => github.com/godbus/dbus v0.0.0-20190623212516-8a1682060722
	github.com/prometheus/client_golang => github.com/prometheus/client_golang v0.9.2
	github.com/sirupsen/logrus => github.com/sirupsen/logrus v1.4.2
	k8s.io/api => k8s.io/kubernetes/staging/src/k8s.io/api v0.0.0-20200408172955-7879fc12a633
	k8s.io/apiextensions-apiserver => k8s.io/kubernetes/staging/src/k8s.io/apiextensions-apiserver v0.0.0-20200408172955-7879fc12a633
	k8s.io/apimachinery => k8s.io/kubernetes/staging/src/k8s.io/apimachinery v0.0.0-20200408172955-7879fc12a633
	k8s.io/apiserver => k8s.io/kubernetes/staging/src/k8s.io/apiserver v0.0.0-20200408172955-7879fc12a633
	k8s.io/cli-runtime => k8s.io/kubernetes/staging/src/k8s.io/cli-runtime v0.0.0-20200408172955-7879fc12a633
	k8s.io/client-go => k8s.io/kubernetes/staging/src/k8s.io/client-go v0.0.0-20200408172955-7879fc12a633
	k8s.io/cloud-provider => k8s.io/kubernetes/staging/src/k8s.io/cloud-provider v0.0.0-20200408172955-7879fc12a633
	k8s.io/cluster-bootstrap => k8s.io/kubernetes/staging/src/k8s.io/cluster-bootstrap v0.0.0-20200408172955-7879fc12a633
	k8s.io/code-generator => k8s.io/kubernetes/staging/src/k8s.io/code-generator v0.0.0-20200408172955-7879fc12a633
	k8s.io/component-base => k8s.io/kubernetes/staging/src/k8s.io/component-base v0.0.0-20200408172955-7879fc12a633
	k8s.io/cri-api => k8s.io/kubernetes/staging/src/k8s.io/cri-api v0.0.0-20200408172955-7879fc12a633
	k8s.io/csi-translation-lib => k8s.io/kubernetes/staging/src/k8s.io/csi-translation-lib v0.0.0-20200408172955-7879fc12a633
	k8s.io/kube-aggregator => k8s.io/kubernetes/staging/src/k8s.io/kube-aggregator v0.0.0-20200408172955-7879fc12a633
	k8s.io/kube-controller-manager => k8s.io/kubernetes/staging/src/k8s.io/kube-controller-manager v0.0.0-20200408172955-7879fc12a633
	k8s.io/kube-proxy => k8s.io/kubernetes/staging/src/k8s.io/kube-proxy v0.0.0-20200408172955-7879fc12a633
	k8s.io/kube-scheduler => k8s.io/kubernetes/staging/src/k8s.io/kube-scheduler v0.0.0-20200408172955-7879fc12a633
	k8s.io/kubectl => k8s.io/kubernetes/staging/src/k8s.io/kubectl v0.0.0-20200408172955-7879fc12a633
	k8s.io/kubelet => k8s.io/kubernetes/staging/src/k8s.io/kubelet v0.0.0-20200408172955-7879fc12a633
	k8s.io/kubernetes => k8s.io/kubernetes v1.18.1
	k8s.io/legacy-cloud-providers => k8s.io/kubernetes/staging/src/k8s.io/legacy-cloud-providers v0.0.0-20200408172955-7879fc12a633
	k8s.io/metrics => k8s.io/kubernetes/staging/src/k8s.io/metrics v0.0.0-20200408172955-7879fc12a633
	k8s.io/sample-apiserver => k8s.io/kubernetes/staging/src/k8s.io/sample-apiserver v0.0.0-20200408172955-7879fc12a633
)
