GO ?= go

export GOPROXY=https://proxy.golang.org
export GOSUMDB=https://sum.golang.org

GO_BUILD ?= $(GO) build
GO_RUN ?= $(GO) run

PROJECT := github.com/cri-o/cri-o
CRIO_INSTANCE := crio_dev
PREFIX ?= ${DESTDIR}/usr/local
BINDIR ?= ${PREFIX}/bin
LIBEXECDIR ?= ${PREFIX}/libexec
MANDIR ?= ${PREFIX}/share/man
ETCDIR ?= ${DESTDIR}/etc
ETCDIR_CRIO ?= ${ETCDIR}/crio
DATAROOTDIR ?= ${PREFIX}/share/containers
BUILDTAGS ?= containers_image_ostree_stub \
			 $(shell hack/apparmor_tag.sh) \
			 $(shell hack/btrfs_installed_tag.sh) \
			 $(shell hack/btrfs_tag.sh) \
			 $(shell hack/libdm_installed.sh) \
			 $(shell hack/libdm_no_deferred_remove_tag.sh) \
			 $(shell hack/openpgp_tag.sh) \
			 $(shell hack/seccomp_tag.sh) \
			 $(shell hack/selinux_tag.sh)
CRICTL_CONFIG_DIR=${DESTDIR}/etc
CONTAINER_RUNTIME ?= podman
BUILD_PATH := $(shell pwd)/build
BUILD_BIN_PATH := ${BUILD_PATH}/bin
COVERAGE_PATH := ${BUILD_PATH}/coverage
JUNIT_PATH := ${BUILD_PATH}/junit
TESTBIN_PATH := ${BUILD_PATH}/test
MOCK_PATH := ${PWD}/test/mocks

BASHINSTALLDIR=${PREFIX}/share/bash-completion/completions
FISHINSTALLDIR=${PREFIX}/share/fish/completions
ZSHINSTALLDIR=${PREFIX}/share/zsh/site-functions
OCIUMOUNTINSTALLDIR=$(PREFIX)/share/oci-umount/oci-umount.d

SELINUXOPT ?= $(shell selinuxenabled 2>/dev/null && echo -Z)

SOURCE_DATE_EPOCH ?= $(shell date +%s)

GO_MD2MAN ?= ${BUILD_BIN_PATH}/go-md2man
GINKGO := ${BUILD_BIN_PATH}/ginkgo
MOCKGEN := ${BUILD_BIN_PATH}/mockgen
GOLANGCI_LINT := ${BUILD_BIN_PATH}/golangci-lint
GO_MOD_OUTDATED := ${BUILD_BIN_PATH}/go-mod-outdated
RELEASE_NOTES := ${BUILD_BIN_PATH}/release-notes
ZEITGEIST := ${BUILD_BIN_PATH}/zeitgeist
SHFMT := ${BUILD_BIN_PATH}/shfmt
SHELLCHECK := ${BUILD_BIN_PATH}/shellcheck
BATS_FILES := $(wildcard test/*.bats)

ifeq ($(shell bash -c '[[ `command -v git` && `git rev-parse --git-dir 2>/dev/null` ]] && echo true'), true)
	COMMIT_NO := $(shell git rev-parse HEAD 2> /dev/null || true)
	GIT_TREE_STATE := $(if $(shell git status --porcelain --untracked-files=no),dirty,clean)
else
	COMMIT_NO := unknown
	GIT_TREE_STATE := unknown
endif

# pass crio CLI options to generate custom configuration options at build time
CONF_OVERRIDES ?=

CROSS_BUILD_TARGETS := \
	bin/crio.cross.windows.amd64 \
	bin/crio.cross.darwin.amd64 \
	bin/crio.cross.linux.amd64

# If GOPATH not specified, use one in the local directory
ifeq ($(GOPATH),)
export GOPATH := $(CURDIR)/_output
unexport GOBIN
endif
GOPKGDIR := $(GOPATH)/src/$(PROJECT)
GOPKGBASEDIR := $(shell dirname "$(GOPKGDIR)")
GO_FILES := $(shell find . -type f -name '*.go' -not -name '*_test.go')

# Update VPATH so make finds .gopathok
VPATH := $(VPATH):$(GOPATH)

# Set DEBUG=1 to enable debug symbols in binaries
DEBUG ?= 0
ifeq ($(DEBUG),0)
SHRINKFLAGS = -s -w
else
GCFLAGS = -gcflags '-N -l'
endif

DEFAULTS_PATH := ""

DATE_FMT = +'%Y-%m-%dT%H:%M:%SZ'
ifdef SOURCE_DATE_EPOCH
    BUILD_DATE ?= $(shell date -u -d "@$(SOURCE_DATE_EPOCH)" "$(DATE_FMT)" 2>/dev/null || date -u -r "$(SOURCE_DATE_EPOCH)" "$(DATE_FMT)" 2>/dev/null || date -u "$(DATE_FMT)")
else
    BUILD_DATE ?= $(shell date -u "$(DATE_FMT)")
endif

BASE_LDFLAGS = ${SHRINKFLAGS} \
	-X ${PROJECT}/internal/pkg/criocli.DefaultsPath=${DEFAULTS_PATH} \
	-X ${PROJECT}/internal/version.buildDate=${BUILD_DATE} \
	-X ${PROJECT}/internal/version.gitCommit=${COMMIT_NO} \
	-X ${PROJECT}/internal/version.gitTreeState=${GIT_TREE_STATE}

GO_LDFLAGS = -ldflags '${BASE_LDFLAGS} ${EXTRA_LDFLAGS}'

all: binaries crio.conf docs

default: help

help:
	@echo "Usage: make <target>"
	@echo
	@echo " * 'install' - Install binaries to system locations"
	@echo " * 'binaries' - Build crio and pinns"
	@echo " * 'release-note' - Generate release note"
	@echo " * 'localintegration' - Execute integration tests"
	@echo " * 'clean' - Clean artifacts"
	@echo " * 'lint' - Execute the source code linter"
	@echo " * 'shfmt' - shell format check and apply diff"
	@echo " * 'shellcheck' - Execute the shellcheck linter"

# Dummy target for marking pattern rules phony
.explicit_phony:

.gopathok:
ifeq ("$(wildcard $(GOPKGDIR))","")
	mkdir -p "$(GOPKGBASEDIR)"
	ln -s "$(CURDIR)" "$(GOPKGDIR)"
endif
	touch "$(GOPATH)/.gopathok"

# See also: .github/workflows/lint.yml
lint: .gopathok ${GOLANGCI_LINT}
	${GOLANGCI_LINT} version
	${GOLANGCI_LINT} linters
	${GOLANGCI_LINT} run

check-log-lines:
	./hack/log-capitalized.sh
	./hack/tree_status.sh

shellfiles: ${SHFMT}
	$(eval SHELLFILES=$(shell ${SHFMT} -f . | grep -v vendor/ | grep -v hack/lib | grep -v hack/build-rpms.sh))

shfmt: shellfiles
	${SHFMT} -ln bash -w -i 4 -d ${SHELLFILES}
	${SHFMT} -ln bats -w -sr -d $(BATS_FILES)

shellcheck: shellfiles ${SHELLCHECK}
	${SHELLCHECK} \
		-P contrib/bundle \
		-P scripts \
		-P test \
		-x \
		${SHELLFILES} ${BATS_FILES}

bin/pinns:
	$(MAKE) -C pinns

test/copyimg/copyimg: $(GO_FILES) .gopathok
	$(GO_BUILD) $(GCFLAGS) $(GO_LDFLAGS) -tags "$(BUILDTAGS)" -o $@ $(PROJECT)/test/copyimg

test/checkseccomp/checkseccomp: $(GO_FILES) .gopathok
	$(GO_BUILD) $(GCFLAGS) $(GO_LDFLAGS) -tags "$(BUILDTAGS)" -o $@ $(PROJECT)/test/checkseccomp

bin/crio: $(GO_FILES) .gopathok
	$(GO_BUILD) $(GCFLAGS) $(GO_LDFLAGS) -tags "$(BUILDTAGS)" -o $@ $(PROJECT)/cmd/crio

bin/crio-status: $(GO_FILES) .gopathok
	$(GO_BUILD) $(GCFLAGS) $(GO_LDFLAGS) -tags "$(BUILDTAGS)" -o $@ $(PROJECT)/cmd/crio-status

build-static:
	$(CONTAINER_RUNTIME) run --rm --privileged -ti -v /:/mnt \
		nixos/nix cp -rfT /nix /mnt/nix
	$(CONTAINER_RUNTIME) run --rm --privileged -ti -v /nix:/nix -v ${PWD}:${PWD} -w ${PWD} \
		nixos/nix nix --print-build-logs --option cores 8 --option max-jobs 8 build --file nix/
	mkdir -p bin
	cp -r result/bin bin/static

release-bundle: clean bin/pinns build-static docs crio.conf bundle

crio.conf: bin/crio
	./bin/crio -d "" --config="" $(CONF_OVERRIDES) config > crio.conf

release:
	${GO_RUN} ./scripts/release

release-notes: ${RELEASE_NOTES}
	${GO_RUN} ./scripts/release-notes \
		--output-path ${BUILD_PATH}/release-notes

dependencies: ${GO_MOD_OUTDATED}
	${GO_RUN} ./scripts/dependencies \
		--output-path ${BUILD_PATH}/dependencies

clean:
ifneq ($(GOPATH),)
	rm -f "$(GOPATH)/.gopathok"
endif
	rm -rf _output
	rm -f docs/*.5 docs/*.8
	rm -fr test/testdata/redis-image
	find . -name \*~ -delete
	find . -name \#\* -delete
	rm -rf bin/
	$(MAKE) -C pinns clean
	rm -f test/copyimg/copyimg
	rm -f test/checkseccomp/checkseccomp
	rm -rf ${BUILD_BIN_PATH}

# the approach here, rather than this target depending on the build targets
# directly, is such that each target should try to build regardless if it
# fails. And return a non-zero exit if _any_ target fails.
local-cross:
	@$(MAKE) --keep-going $(CROSS_BUILD_TARGETS)

bin/crio.cross.%: .gopathok .explicit_phony
	@echo "==> make $@"; \
	TARGET="$*"; \
	GOOS="$${TARGET%%.*}" \
	GOARCH="$${TARGET##*.}" \
	$(GO_BUILD) $(GO_LDFLAGS) -tags "containers_image_openpgp btrfs_noversion" -o "$@" $(PROJECT)/cmd/crio

nixpkgs:
	@nix run -f channel:nixos-20.09 nix-prefetch-git -c nix-prefetch-git \
		--no-deepClone https://github.com/nixos/nixpkgs > nix/nixpkgs.json


define go-build
	$(shell cd `pwd` && $(GO_BUILD) -o $(BUILD_BIN_PATH)/$(shell basename $(1)) $(1))
	@echo > /dev/null
endef

${GO_MD2MAN}:
	$(call go-build,./vendor/github.com/cpuguy83/go-md2man)

${GINKGO}:
	$(call go-build,./vendor/github.com/onsi/ginkgo/ginkgo)

${MOCKGEN}:
	$(call go-build,./vendor/github.com/golang/mock/mockgen)

${RELEASE_NOTES}:
	$(call go-build,./vendor/k8s.io/release/cmd/release-notes)

${SHFMT}:
	$(call go-build,./vendor/mvdan.cc/sh/v3/cmd/shfmt)

${GO_MOD_OUTDATED}:
	$(call go-build,./vendor/github.com/psampaz/go-mod-outdated)

${ZEITGEIST}:
	$(call go-build,./vendor/sigs.k8s.io/zeitgeist)

${GOLANGCI_LINT}:
	export VERSION=v1.40.1 \
		URL=https://raw.githubusercontent.com/golangci/golangci-lint \
		BINDIR=${BUILD_BIN_PATH} && \
	curl -sSfL $$URL/$$VERSION/install.sh | sh -s $$VERSION

${SHELLCHECK}:
	mkdir -p ${BUILD_BIN_PATH} && \
	VERSION=v0.7.0 \
	URL=https://github.com/koalaman/shellcheck/releases/download/$$VERSION/shellcheck-$$VERSION.linux.x86_64.tar.xz \
	SHA256SUM=c37d4f51e26ec8ab96b03d84af8c050548d7288a47f755ffb57706c6c458e027 && \
	curl -sSfL $$URL | tar xfJ - -C ${BUILD_BIN_PATH} --strip 1 shellcheck-$$VERSION/shellcheck && \
	sha256sum ${SHELLCHECK} | grep -q $$SHA256SUM

vendor: export GOSUMDB :=
vendor:
	$(GO) mod tidy
	$(GO) mod vendor
	$(GO) mod verify

check-vendor: vendor
	./hack/tree_status.sh

testunit: ${GINKGO}
	rm -rf ${COVERAGE_PATH} && mkdir -p ${COVERAGE_PATH}
	rm -rf ${JUNIT_PATH} && mkdir -p ${JUNIT_PATH}
	ACK_GINKGO_DEPRECATIONS=1.16.0 \
	${BUILD_BIN_PATH}/ginkgo \
		${TESTFLAGS} \
		-r \
		--trace \
		--cover \
		--covermode atomic \
		--outputdir ${COVERAGE_PATH} \
		--coverprofile coverprofile \
		--tags "test $(BUILDTAGS)" \
		$(GO_MOD_VENDOR) \
		--succinct
	$(GO) tool cover -html=${COVERAGE_PATH}/coverprofile -o ${COVERAGE_PATH}/coverage.html
	$(GO) tool cover -func=${COVERAGE_PATH}/coverprofile | sed -n 's/\(total:\).*\([0-9][0-9].[0-9]\)/\1 \2/p'
	for f in $$(find . -name "*_junit.xml"); do \
		mkdir -p $(JUNIT_PATH)/$$(dirname $$f) ;\
		mv $$f $(JUNIT_PATH)/$$(dirname $$f) ;\
	done

testunit-bin:
	mkdir -p ${TESTBIN_PATH}
	for PACKAGE in `$(GO) list ./...`; do \
		go test $$PACKAGE \
			--tags "test $(BUILDTAGS)" \
			--gcflags '-N' -c -o ${TESTBIN_PATH}/$$(basename $$PACKAGE) ;\
	done

mockgen: \
	mock-containerstorage \
	mock-criostorage \
	mock-lib-config \
	mock-oci \
	mock-image-types \
	mock-ocicni-types \
	mock-metrics

mock-containerstorage: ${MOCKGEN}
	${MOCKGEN} \
		-package containerstoragemock \
		-destination ${MOCK_PATH}/containerstorage/containerstorage.go \
		github.com/containers/storage Store

mock-cmdrunner: ${MOCKGEN}
	${MOCKGEN} \
		-package cmdrunnermock \
		-destination ${MOCK_PATH}/cmdrunner/cmdrunner.go \
		github.com/cri-o/cri-o/utils/cmdrunner CommandRunner

mock-criostorage: ${MOCKGEN}
	${MOCKGEN} \
		-package criostoragemock \
		-destination ${MOCK_PATH}/criostorage/criostorage.go \
		github.com/cri-o/cri-o/internal/storage ImageServer,RuntimeServer

mock-lib-config: ${MOCKGEN}
	${MOCKGEN} \
		-package libconfigmock \
		-destination ${MOCK_PATH}/lib/lib.go \
		github.com/cri-o/cri-o/pkg/config Iface

mock-oci: ${MOCKGEN}
	${MOCKGEN} \
		-package ocimock \
		-destination ${MOCK_PATH}/oci/oci.go \
		github.com/cri-o/cri-o/internal/oci RuntimeImpl

mock-image-types: ${MOCKGEN}
	${BUILD_BIN_PATH}/mockgen \
		-package imagetypesmock \
		-destination ${MOCK_PATH}/containers/image/v5/types.go \
		github.com/containers/image/v5/types ImageCloser

mock-ocicni-types: ${MOCKGEN}
	${BUILD_BIN_PATH}/mockgen \
		-package ocicnitypesmock \
		-destination ${MOCK_PATH}/ocicni/types.go \
		github.com/cri-o/ocicni/pkg/ocicni CNIPlugin

mock-metrics: ${MOCKGEN}
	${MOCKGEN} \
		-package metricsmock \
		-destination ${MOCK_PATH}/metrics/metrics.go \
		github.com/cri-o/cri-o/server/metrics Impl

codecov: SHELL := $(shell which bash)
codecov:
	bash <(curl -s https://codecov.io/bash) -f ${COVERAGE_PATH}/coverprofile

localintegration: clean binaries test-binaries
	./test/test_runner.sh ${TESTFLAGS}

binaries: bin/crio bin/crio-status bin/pinns
test-binaries: test/copyimg/copyimg test/checkseccomp/checkseccomp

MANPAGES_MD := $(wildcard docs/*.md)
MANPAGES    := $(MANPAGES_MD:%.md=%)

docs/%.5: docs/%.5.md .gopathok ${GO_MD2MAN}
	(${GO_MD2MAN} -in $< -out $@.tmp && touch $@.tmp && mv $@.tmp $@) || \
		(${GO_MD2MAN} -in $< -out $@.tmp && touch $@.tmp && mv $@.tmp $@)

docs/%.8: docs/%.8.md .gopathok ${GO_MD2MAN}
	(${GO_MD2MAN} -in $< -out $@.tmp && touch $@.tmp && mv $@.tmp $@) || \
		(${GO_MD2MAN} -in $< -out $@.tmp && touch $@.tmp && mv $@.tmp $@)

completions-generation:
	bin/crio complete bash > completions/bash/crio
	bin/crio complete fish > completions/fish/crio.fish
	bin/crio complete zsh  > completions/zsh/_crio
	bin/crio-status complete bash > completions/bash/crio-status
	bin/crio-status complete fish > completions/fish/crio-status.fish
	bin/crio-status complete zsh  > completions/zsh/_crio-status

docs: $(MANPAGES)

docs-generation:
	bin/crio-status md  > docs/crio-status.8.md
	bin/crio-status man > docs/crio-status.8
	bin/crio -d "" --config="" md  > docs/crio.8.md
	bin/crio -d "" --config="" man > docs/crio.8

bundle:
	contrib/bundle/build

bundle-test:
	sudo contrib/bundle/test

bundle-test-e2e:
	sudo contrib/bundle/test-e2e

bundles:
	contrib/bundle/build amd64
	contrib/bundle/build arm64

get-script:
	sed -i '/# INCLUDE/q' scripts/get
	cat contrib/bundle/install-paths contrib/bundle/install >> scripts/get

verify-dependencies: ${ZEITGEIST}
	${BUILD_BIN_PATH}/zeitgeist validate --local-only --base-path . --config dependencies.yaml

install: .gopathok install.bin install.man install.completions install.systemd install.config

install.bin-nobuild:
	install ${SELINUXOPT} -D -m 755 bin/crio $(BINDIR)/crio
	install ${SELINUXOPT} -D -m 755 bin/crio-status $(BINDIR)/crio-status
	install ${SELINUXOPT} -D -m 755 bin/pinns $(BINDIR)/pinns

install.bin: binaries install.bin-nobuild

install.man-nobuild:
	install ${SELINUXOPT} -d -m 755 $(MANDIR)/man5
	install ${SELINUXOPT} -d -m 755 $(MANDIR)/man8
	install ${SELINUXOPT} -m 644 $(filter %.5,$(MANPAGES)) -t $(MANDIR)/man5
	install ${SELINUXOPT} -m 644 $(filter %.8,$(MANPAGES)) -t $(MANDIR)/man8

install.man: $(MANPAGES) install.man-nobuild

install.config-nobuild:
	install ${SELINUXOPT} -d $(DATAROOTDIR)/oci/hooks.d
	install ${SELINUXOPT} -d $(ETCDIR_CRIO)/crio.conf.d
	install ${SELINUXOPT} -D -m 644 crio.conf $(ETCDIR_CRIO)/crio.conf
	install ${SELINUXOPT} -D -m 644 crio-umount.conf $(OCIUMOUNTINSTALLDIR)/crio-umount.conf
	install ${SELINUXOPT} -D -m 644 crictl.yaml $(CRICTL_CONFIG_DIR)

install.config: crio.conf install.config-nobuild

install.completions:
	install ${SELINUXOPT} -d -m 755 ${BASHINSTALLDIR}
	install ${SELINUXOPT} -d -m 755 ${FISHINSTALLDIR}
	install ${SELINUXOPT} -d -m 755 ${ZSHINSTALLDIR}
	install ${SELINUXOPT} -D -m 644 -t ${BASHINSTALLDIR} completions/bash/crio
	install ${SELINUXOPT} -D -m 644 -t ${FISHINSTALLDIR} completions/fish/crio.fish
	install ${SELINUXOPT} -D -m 644 -t ${ZSHINSTALLDIR}  completions/zsh/_crio
	install ${SELINUXOPT} -D -m 644 -t ${BASHINSTALLDIR} completions/bash/crio-status
	install ${SELINUXOPT} -D -m 644 -t ${FISHINSTALLDIR} completions/fish/crio-status.fish
	install ${SELINUXOPT} -D -m 644 -t ${ZSHINSTALLDIR}  completions/zsh/_crio-status

install.systemd:
	install ${SELINUXOPT} -D -m 644 contrib/systemd/crio.service $(PREFIX)/lib/systemd/system/crio.service
	ln -sf crio.service $(PREFIX)/lib/systemd/system/cri-o.service
	install ${SELINUXOPT} -D -m 644 contrib/systemd/crio-shutdown.service $(PREFIX)/lib/systemd/system/crio-shutdown.service
	install ${SELINUXOPT} -D -m 644 contrib/systemd/crio-wipe.service $(PREFIX)/lib/systemd/system/crio-wipe.service

uninstall:
	rm -f $(BINDIR)/crio
	rm -f $(BINDIR)/crio-status
	rm -f $(BINDIR)/pinns
	for i in $(filter %.5,$(MANPAGES)); do \
		rm -f $(MANDIR)/man5/$$(basename $${i}); \
	done
	for i in $(filter %.8,$(MANPAGES)); do \
		rm -f $(MANDIR)/man8/$$(basename $${i}); \
	done
	rm -f ${BASHINSTALLDIR}/crio
	rm -f ${FISHINSTALLDIR}/crio.fish
	rm -f ${ZSHINSTALLDIR}/_crio
	rm -f ${BASHINSTALLDIR}/crio-status
	rm -f ${FISHINSTALLDIR}/crio-status.fish
	rm -f ${ZSHINSTALLDIR}/_crio-status
	rm -f $(PREFIX)/lib/systemd/system/crio-wipe.service
	rm -f $(PREFIX)/lib/systemd/system/crio-shutdown.service
	rm -f $(PREFIX)/lib/systemd/system/crio.service
	rm -f $(PREFIX)/lib/systemd/system/cri-o.service
	rm -rf $(DATAROOTDIR)/oci/hooks.d
	rm -f $(ETCDIR_CRIO)/crio.conf
	rm -rf $(ETCDIR_CRIO)/crio.conf.d
	rm -f $(OCIUMOUNTINSTALLDIR)/crio-umount.conf
	rm -f $(CRICTL_CONFIG_DIR)/crictl.yaml

docs-validation:
	$(GO_RUN) -tags "$(BUILDTAGS)" ./test/docs-validation

release-branch-forward:
	$(GO_RUN) ./scripts/release-branch-forward

upload-artifacts:
	./scripts/upload-artifacts

bin/metrics-exporter:
	$(GO_BUILD) -o $@ \
		-ldflags '-linkmode external -extldflags "-static -lm"' \
		-tags netgo \
		$(PROJECT)/contrib/metrics-exporter

metrics-exporter: bin/metrics-exporter
	$(CONTAINER_RUNTIME) build . \
		-f contrib/metrics-exporter/Containerfile \
		-t quay.io/crio/metrics-exporter:latest

.PHONY: \
	.explicit_phony \
	git-validation \
	binaries \
	bundle \
	bundles \
	bundle-test \
	build-static \
	clean \
	completions \
	config \
	default \
	docs \
	docs-validation \
	help \
	install \
	lint \
	local-cross \
	nixpkgs \
	release-bundle \
	shellfiles \
	shfmt \
	release-branch-forward \
	shellcheck \
	testunit \
	testunit-bin \
	test-images \
	uninstall \
	vendor \
	check-vendor \
	bin/pinns \
	dependencies \
	upload-artifacts \
	bin/metrics-exporter \
	metrics-exporter \
	release \
	get-script \
	check-log-lines \
	verify-dependencies
