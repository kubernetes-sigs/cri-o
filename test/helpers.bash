#!/usr/bin/env bash

# Root directory of integration tests.
INTEGRATION_ROOT=${INTEGRATION_ROOT:-$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")}

# Test data path.
TESTDATA="${INTEGRATION_ROOT}/testdata"

# Root directory of the repository.
CRIO_ROOT=${CRIO_ROOT:-$(
    cd "$INTEGRATION_ROOT/.." || exit
    pwd -P
)}

# Path to the crio binary.
CRIO_BINARY=${CRIO_BINARY:-crio}
CRIO_BINARY_PATH=${CRIO_BINARY_PATH:-${CRIO_ROOT}/bin/$CRIO_BINARY}

# Path to the crio-status binary.
CRIO_STATUS_BINARY_PATH=${CRIO_STATUS_BINARY_PATH:-${CRIO_ROOT}/bin/crio-status}

# Path to the pinns binary
PINNS_BINARY_PATH=${PINNS_BINARY_PATH:-${CRIO_ROOT}/bin/pinns}

# Path of the crictl binary.
CRICTL_PATH=$(command -v crictl || true)
CRICTL_BINARY=${CRICTL_PATH:-/usr/bin/crictl}
# Path of the conmon binary set as a variable to allow overwriting.
CONMON_BINARY=${CONMON_BINARY:-$(command -v conmon)}
# Cgroup for the conmon process
CONTAINER_CONMON_CGROUP=${CONTAINER_CONMON_CGROUP:-pod}
# Path of the default seccomp profile.
CONTAINER_SECCOMP_PROFILE=${CONTAINER_SECCOMP_PROFILE:-${CRIO_ROOT}/vendor/github.com/seccomp/containers-golang/seccomp.json}
CONTAINER_UID_MAPPINGS=${CONTAINER_UID_MAPPINGS:-}
CONTAINER_GID_MAPPINGS=${CONTAINER_GID_MAPPINGS:-}
OVERRIDE_OPTIONS=${OVERRIDE_OPTIONS:-}
# Runtime
CONTAINER_RUNTIME=${CONTAINER_RUNTIME:-runc}
CONTAINER_DEFAULT_RUNTIME=${CONTAINER_DEFAULT_RUNTIME:-runc}
RUNTIME_NAME=${RUNTIME_NAME:-runc}
RUNTIME_PATH=$(command -v "$CONTAINER_RUNTIME" || true)
RUNTIME_BINARY=${RUNTIME_PATH:-$(command -v runc)}
RUNTIME_TYPE=${RUNTIME_TYPE:-oci}
if [[ $CONTAINER_RUNTIME == "kata-runtime" ]]; then
    export RUNTIME_NAME="$CONTAINER_RUNTIME"
    export CONTAINER_DEFAULT_RUNTIME="$RUNTIME_NAME"
fi
# Path of the apparmor_parser binary.
APPARMOR_PARSER_BINARY=${APPARMOR_PARSER_BINARY:-/sbin/apparmor_parser}
# Path of the apparmor profile for test.
APPARMOR_TEST_PROFILE_PATH=${APPARMOR_TEST_PROFILE_PATH:-${TESTDATA}/apparmor_test_deny_write}
# Path of the apparmor profile for unloading crio-default.
FAKE_CRIO_DEFAULT_PROFILE_PATH=${FAKE_CRIO_DEFAULT_PROFILE_PATH:-${TESTDATA}/fake_crio_default}
# Name of the default apparmor profile.
FAKE_CRIO_DEFAULT_PROFILE_NAME=${FAKE_CRIO_DEFAULT_PROFILE_NAME:-crio-default-fake}
# Name of the apparmor profile for test.
APPARMOR_TEST_PROFILE_NAME=${APPARMOR_TEST_PROFILE_NAME:-apparmor-test-deny-write}
# Path of boot config.
BOOT_CONFIG_FILE_PATH=${BOOT_CONFIG_FILE_PATH:-/boot/config-$(uname -r)}
# Path of apparmor parameters file.
APPARMOR_PARAMETERS_FILE_PATH=${APPARMOR_PARAMETERS_FILE_PATH:-/sys/module/apparmor/parameters/enabled}
# Path of the copyimg binary.
COPYIMG_BINARY=${COPYIMG_BINARY:-${CRIO_ROOT}/test/copyimg/copyimg}
# Path of tests artifacts.
ARTIFACTS_PATH=${ARTIFACTS_PATH:-${CRIO_ROOT}/.artifacts}
# Path of the checkseccomp binary.
CHECKSECCOMP_BINARY=${CHECKSECCOMP_BINARY:-${CRIO_ROOT}/test/checkseccomp/checkseccomp}
# The default log directory where all logs will go unless directly specified by the kubelet
DEFAULT_LOG_PATH=${DEFAULT_LOG_PATH:-/var/log/crio/pods}
# Cgroup manager to be used
CONTAINER_CGROUP_MANAGER=${CONTAINER_CGROUP_MANAGER:-systemd}
# Image volumes handling
CONTAINER_IMAGE_VOLUMES=${CONTAINER_IMAGE_VOLUMES:-mkdir}
# Container pids limit
CONTAINER_PIDS_LIMIT=${CONTAINER_PIDS_LIMIT:-1024}
# Log size max limit
CONTAINER_LOG_SIZE_MAX=${CONTAINER_LOG_SIZE_MAX:--1}
# Stream Port
STREAM_PORT=${STREAM_PORT:-10010}
# Metrics Port
CONTAINER_METRICS_PORT=${CONTAINER_METRICS_PORT:-9090}

POD_IPV4_CIDR="10.88.0.0/16"
POD_IPV4_CIDR_START="10.88"
POD_IPV4_DEF_ROUTE="0.0.0.0/0"

POD_IPV6_CIDR="1100:200::/24"
POD_IPV6_CIDR_START="1100:200::"
POD_IPV6_DEF_ROUTE="1100:200::1/24"

IMAGES=(
    k8s.gcr.io/pause:3.2
    quay.io/crio/busybox:latest
    quay.io/crio/fedora-ping:latest
    quay.io/crio/image-volume-test:latest
    quay.io/crio/oom:latest
    quay.io/crio/redis:alpine
    quay.io/crio/stderr-test:latest
)

function img2dir() {
    local dir
    dir=$(echo "$@" | sed -e 's|^.*/||' -e 's/:.*$//' -e 's/-/_/' -e 's/$/-image/')
    echo "$ARTIFACTS_PATH/$dir"
}

function get_img() {
    local img="docker://$1" dir
    dir="$(img2dir "$img")"

    if ! [ -d "$dir" ]; then
        mkdir -p "$dir"
        if ! "$COPYIMG_BINARY" \
            --import-from="$img" \
            --export-to="dir:$dir" \
            --signature-policy="$INTEGRATION_ROOT"/policy.json; then
            echo "Error pulling $img" >&2
            rm -fr "$dir"
            exit 1
        fi
    fi
}

for img in "${IMAGES[@]}"; do
    get_img "$img"
done

function setup_test() {
    TESTDIR=$(mktemp -d)
    RANDOM_CNI_NETWORK=${TESTDIR: -10}

    # Setup default hooks dir
    HOOKSDIR=$TESTDIR/hooks
    mkdir "$HOOKSDIR"

    HOOKSCHECK=$TESTDIR/hookscheck
    CONTAINER_EXITS_DIR=$TESTDIR/containers/exits
    CONTAINER_ATTACH_SOCKET_DIR=$TESTDIR/containers

    MOUNT_PATH="$TESTDIR/secrets"
    mkdir "$MOUNT_PATH"
    MOUNT_FILE="$MOUNT_PATH/test.txt"
    touch "$MOUNT_FILE"
    echo "Testing secrets mounts!" >"$MOUNT_FILE"

    # Setup default secrets mounts
    mkdir "$TESTDIR/containers"
    touch "$TESTDIR/containers/mounts.conf"
    echo "$TESTDIR/rhel/secrets:/run/secrets" >"$TESTDIR/containers/mounts.conf"
    echo "$MOUNT_PATH:/container/path1" >>"$TESTDIR/containers/mounts.conf"
    mkdir -p "$TESTDIR/rhel/secrets"
    touch "$TESTDIR/rhel/secrets/test.txt"
    echo "Testing secrets mounts. I am mounted!" >"$TESTDIR/rhel/secrets/test.txt"
    mkdir -p "$TESTDIR/symlink/target"
    touch "$TESTDIR/symlink/target/key.pem"
    ln -s "$TESTDIR/symlink/target" "$TESTDIR/rhel/secrets/mysymlink"

    # We may need to set some default storage options.
    case "$(stat -f -c %T "$TESTDIR")" in
    aufs)
        # None of device mapper, overlay, or aufs can be used dependably over aufs, and of course btrfs and zfs can't,
        # and we have to explicitly specify the "vfs" driver in order to use it, so do that now.
        STORAGE_OPTIONS=${STORAGE_OPTIONS:--s vfs}
        ;;
    *)
        STORAGE_OPTIONS=${STORAGE_OPTIONS:-}
        ;;
    esac

    if [ -e /usr/sbin/selinuxenabled ] && /usr/sbin/selinuxenabled; then
        # shellcheck disable=SC1091
        . /etc/selinux/config
        filelabel=$(awk -F'"' '/^file.*=.*/ {print $2}' "/etc/selinux/${SELINUXTYPE}/contexts/lxc_contexts")
        chcon -R "$filelabel" "$TESTDIR"
    fi
    CRIO_SOCKET="$TESTDIR/crio.sock"
    CRIO_CONFIG_DIR="$TESTDIR/crio.conf.d"
    mkdir "$CRIO_CONFIG_DIR"
    CRIO_CONFIG="$TESTDIR/crio.conf"
    CRIO_CNI_CONFIG="$TESTDIR/cni/net.d/"
    CRIO_LOG="$TESTDIR/crio.log"

    # Copy all the CNI dependencies around to ensure encapsulated tests
    CRIO_CNI_PLUGIN="$TESTDIR/cni-bin"
    mkdir "$CRIO_CNI_PLUGIN"
    cp /opt/cni/bin/* "$CRIO_CNI_PLUGIN"
    cp "$INTEGRATION_ROOT"/cni_plugin_helper.bash "$CRIO_CNI_PLUGIN"
    sed -i "s;%TEST_DIR%;$TESTDIR;" "$CRIO_CNI_PLUGIN"/cni_plugin_helper.bash

    # Configure crictl to not try pulling images on create/run,
    # as $IMAGES are already preloaded, and eliminating network
    # interaction results in less flakes when creating containers.
    #
    # A test case that requires an image not listed in $IMAGES
    # should either do an explicit "crictl pull", or use --with-pull.
    crictl config \
        --set pull-image-on-create=false \
        --set disable-pull-on-run=true \
        2>/dev/null || true
    #   ^^^^^^^^^^^^^^^^^^^ TODO: remove the line above once crictl is updated
    # to >= 1.19. It is not a problem if this setting is not working, and since
    # some CI jobs (kata) use older crictl, set this on a best-effort basis.

    PATH=$PATH:$TESTDIR
}

# Run crio using the binary specified by $CRIO_BINARY_PATH.
# This must ONLY be run on engines created with `start_crio`.
function crio() {
    "$CRIO_BINARY_PATH" --listen "$CRIO_SOCKET" "$@"
}

# Run crictl using the binary specified by $CRICTL_BINARY.
function crictl() {
    "$CRICTL_BINARY" -r "unix://$CRIO_SOCKET" -i "unix://$CRIO_SOCKET" "$@"
}

# Run the runtime binary with the specified RUNTIME_ROOT
function runtime() {
    "$CONTAINER_RUNTIME" --root "$RUNTIME_ROOT" "$@"
}

# Communicate with Docker on the host machine.
# Should rarely use this.
function docker_host() {
    command docker "$@"
}

# Retry a command $1 times until it succeeds. Wait $2 seconds between retries.
function retry() {
    local attempts=$1
    shift
    local delay=$1
    shift
    local i

    for ((i = 0; i < attempts; i++)); do
        if "$@"; then
            return 0
        fi
        sleep "$delay"
    done

    echo "Command \"$*\" failed $attempts times"
    false
}

# Waits until crio becomes reachable.
function wait_until_reachable() {
    retry 15 1 crictl info
}

function copyimg() {
    # Don't forget: copyimg and crio have their own default drivers,
    # so if you override any, you probably need to override them all.

    # shellcheck disable=SC2086
    "$COPYIMG_BINARY" \
        --root "$TESTDIR/crio" \
        --runroot "$TESTDIR/crio-run" \
        --signature-policy="$INTEGRATION_ROOT"/policy.json \
        $STORAGE_OPTIONS \
        "$@"
}

function setup_img() {
    local name="$1" dir
    dir="$(img2dir "$name")"

    copyimg --image-name="$name" --import-from="dir:$dir"
}

function setup_crio() {
    apparmor=""
    if [[ -n "$1" ]]; then
        apparmor="$1"
    fi

    for img in "${IMAGES[@]}"; do
        setup_img "$img"
    done

    # Prepare the CNI configuration files, we're running with non host
    # networking by default
    CNI_DEFAULT_NETWORK=crio
    netfunc="prepare_network_conf"
    if [[ -n "$2" ]]; then
        netfunc="$2"
        CNI_DEFAULT_NETWORK="crio-$RANDOM_CNI_NETWORK"
    fi
    RUNTIME_ROOT="$TESTDIR/crio-runtime-root"

    # shellcheck disable=SC2086
    "$CRIO_BINARY_PATH" \
        --hooks-dir="$HOOKSDIR" \
        --apparmor-profile "$apparmor" \
        --cgroup-manager "$CONTAINER_CGROUP_MANAGER" \
        --conmon "$CONMON_BINARY" \
        --listen "$CRIO_SOCKET" \
        --registry "quay.io" \
        --registry "docker.io" \
        --runtimes "$RUNTIME_NAME:$RUNTIME_BINARY:$RUNTIME_ROOT:$RUNTIME_TYPE" \
        -r "$TESTDIR/crio" \
        --runroot "$TESTDIR/crio-run" \
        --cni-default-network "$CNI_DEFAULT_NETWORK" \
        --cni-config-dir "$CRIO_CNI_CONFIG" \
        --cni-plugin-dir "$CRIO_CNI_PLUGIN" \
        --pinns-path "$PINNS_BINARY_PATH" \
        $STORAGE_OPTIONS \
        -c "" \
        -d "" \
        $OVERRIDE_OPTIONS \
        config >"$CRIO_CONFIG"
    sed -r -e 's/^(#)?root =/root =/g' -e 's/^(#)?runroot =/runroot =/g' -e 's/^(#)?storage_driver =/storage_driver =/g' -e '/^(#)?storage_option = (\[)?[ \t]*$/,/^#?$/s/^(#)?//g' -e '/^(#)?registries = (\[)?[ \t]*$/,/^#?$/s/^(#)?//g' -e '/^(#)?default_ulimits = (\[)?[ \t]*$/,/^#?$/s/^(#)?//g' -i "$CRIO_CONFIG"
    # make sure we don't run with nodev, or else mounting a readonly rootfs will fail: https://github.com/cri-o/cri-o/issues/1929#issuecomment-474240498
    sed -r -e 's/nodev(,)?//g' -i "$CRIO_CONFIG"
    sed -i -e 's;\(container_exits_dir =\) \(.*\);\1 "'"$CONTAINER_EXITS_DIR"'";g' "$CRIO_CONFIG"
    sed -i -e 's;\(container_attach_socket_dir =\) \(.*\);\1 "'"$CONTAINER_ATTACH_SOCKET_DIR"'";g' "$CRIO_CONFIG"
    ${netfunc}
}

function check_images() {
    local img json list

    # check that images are there
    json=$(crictl images -o json)
    [ -n "$json" ]
    list=$(jq -r '.images[] | .repoTags[]' <<<"$json")
    for img in "${IMAGES[@]}"; do
        if [[ "$list" != *"$img"* ]]; then
            echo "Image $img is not present but it should!" >&2
            exit 1
        fi
    done

    # these two variables are used by a few tests
    eval "$(jq -r '.images[] |
        select(.repoTags[0] == "quay.io/crio/redis:alpine") |
        "REDIS_IMAGEID=" + .id + "\n" +
	"REDIS_IMAGEREF=" + .repoDigests[0]' <<<"$json")"
}

function start_crio_no_setup() {
    "$CRIO_BINARY_PATH" \
        --default-mounts-file "$TESTDIR/containers/mounts.conf" \
        -l debug \
        -c "$CRIO_CONFIG" \
        -d "$CRIO_CONFIG_DIR" \
        &> >(tee "$CRIO_LOG") &
    CRIO_PID=$!
    wait_until_reachable
}

# Start crio.
# shellcheck disable=SC2120
function start_crio() {
    setup_crio "$@"
    start_crio_no_setup
    check_images
}

function check_journald() {
    if ! pkg-config --exists libsystemd-journal; then
        if ! pkg-config --exists libsystemd; then
            echo "1"
            return
        fi
    fi

    if ! journalctl --version; then
        echo "1"
        return
    fi
    echo "0"
}

# Check whether metrics port is listening
function check_metrics_port() {
    if ! netstat -lanp | grep "$1" >/dev/null; then
        echo "1"
        return
    fi
    echo "0"
}

function cleanup_ctrs() {
    if output=$(crictl ps --quiet); then
        if [ "$output" != "" ]; then
            printf '%s\n' "$output" | while IFS= read -r line; do
                crictl stop "$line"
                crictl rm "$line"
            done
        fi
    fi
    rm -f "$HOOKSCHECK"
}

function cleanup_images() {
    if output=$(crictl images --quiet); then
        if [ "$output" != "" ]; then
            printf '%s\n' "$output" | while IFS= read -r line; do
                crictl rmi "$line"
            done
        fi
    fi
}

function cleanup_pods() {
    if output=$(crictl pods --quiet); then
        if [ "$output" != "" ]; then
            printf '%s\n' "$output" | while IFS= read -r line; do
                crictl stopp "$line"
                crictl rmp "$line"
            done
        fi
    fi
}

function stop_crio_no_clean() {
    if [ -n "${CRIO_PID+x}" ]; then
        kill "$CRIO_PID" >/dev/null 2>&1
        wait "$CRIO_PID"
        unset CRIO_PID
    fi
}

# Stop crio.
function stop_crio() {
    stop_crio_no_clean
    cleanup_network_conf
}

function restart_crio() {
    if [ "$CRIO_PID" != "" ]; then
        kill "$CRIO_PID" >/dev/null 2>&1
        wait "$CRIO_PID"
        start_crio
    else
        echo "you must start crio first"
        exit 1
    fi
}

function cleanup_lvm() {
    if [ -n "${LVM_DEVICE+x}" ]; then
        lvm lvremove -y storage/thinpool
        lvm vgremove -y storage
        lvm pvremove -y "$LVM_DEVICE"
    fi
}

function cleanup_test() {
    cleanup_ctrs
    cleanup_pods
    stop_crio
    cleanup_lvm
    rm -r "$TESTDIR" || true
}

function load_apparmor_profile() {
    "$APPARMOR_PARSER_BINARY" -r "$1"
}

function remove_apparmor_profile() {
    "$APPARMOR_PARSER_BINARY" -R "$1"
}

function is_apparmor_enabled() {
    if [[ -f "$APPARMOR_PARAMETERS_FILE_PATH" ]]; then
        out=$(cat "$APPARMOR_PARAMETERS_FILE_PATH")
        if [[ "$out" =~ "Y" ]]; then
            echo 1
            return
        fi
    fi
    echo 0
}

function prepare_network_conf() {
    mkdir -p "$CRIO_CNI_CONFIG"
    cat >"$CRIO_CNI_CONFIG/10-crio.conf" <<-EOF
{
    "cniVersion": "0.3.1",
    "name": "crio",
    "type": "bridge",
    "bridge": "cni0",
    "isGateway": true,
    "ipMasq": true,
    "ipam": {
        "type": "host-local",
        "routes": [
            { "dst": "$POD_IPV4_DEF_ROUTE" },
            { "dst": "$POD_IPV6_DEF_ROUTE" }
        ],
        "ranges": [
            [{ "subnet": "$POD_IPV4_CIDR" }],
            [{ "subnet": "$POD_IPV6_CIDR" }]
        ]
    }
}
EOF

    echo 0
}

function write_plugin_test_args_network_conf() {
    mkdir -p "$CRIO_CNI_CONFIG"
    cat >"$CRIO_CNI_CONFIG/10-plugin-test-args.conf" <<-EOF
{
    "cniVersion": "0.3.1",
    "name": "crio-$RANDOM_CNI_NETWORK",
    "type": "cni_plugin_helper.bash",
    "bridge": "cni0",
    "isGateway": true,
    "ipMasq": true,
    "ipam": {
        "type": "host-local",
        "subnet": "$POD_IPV4_CIDR",
        "routes": [
            { "dst": "$POD_IPV4_DEF_ROUTE"  }
        ]
    }
}
EOF

    if [[ -n "$1" ]]; then
        echo "DEBUG_ARGS=$1" >"$TESTDIR"/cni_plugin_helper_input.env
    fi

    echo 0
}

function prepare_plugin_test_args_network_conf() {
    write_plugin_test_args_network_conf
}

function prepare_plugin_test_args_network_conf_malformed_result() {
    write_plugin_test_args_network_conf "malformed-result"
}

function parse_pod_ip() {
    inet=$(crictl exec --sync "$1" ip addr show dev eth0 scope global 2>&1 | grep "$2")
    echo "$inet" | sed -n 's;.*\('"$3"'.*\)/.*;\1;p'
}

function parse_pod_ipv4() {
    parse_pod_ip "$1" 'inet ' $POD_IPV4_CIDR_START
}

function parse_pod_ipv6() {
    parse_pod_ip "$1" inet6 $POD_IPV6_CIDR_START
}

function get_host_ip() {
    gateway_dev=$(ip -o route show default $POD_IPV4_DEF_ROUTE | sed 's/.*dev \([^[:space:]]*\).*/\1/')
    [ "$gateway_dev" ]
    ip -o -4 addr show dev "$gateway_dev" scope global | sed 's/.*inet \([0-9.]*\).*/\1/'
}

function ping_pod() {
    ipv4=$(parse_pod_ipv4 "$1")
    ping -W 1 -c 5 "$ipv4"

    ipv6=$(parse_pod_ipv6 "$1")
    ping6 -W 1 -c 5 "$ipv6"
}

function ping_pod_from_pod() {
    ipv4=$(parse_pod_ipv4 "$1")
    crictl exec --sync "$2" ping -W 1 -c 2 "$ipv4"

    # since RHEL kernels don't mirror ipv4.ip_forward sysctl to ipv6, this fails
    # in such an environment without giving all containers NET_RAW capability
    # rather than reducing the security of the tests for all cases, skip this check
    # instead
    if (grep -i 'Red Hat\|CentOS' /etc/redhat-release | grep " 7"); then
        return
    fi
    ipv6=$(parse_pod_ipv6 "$1")
    crictl exec --sync "$2" ping6 -W 1 -c 2 "$ipv6"
}

function cleanup_network_conf() {
    rm -rf "$CRIO_CNI_CONFIG"
    echo 0
}

function temp_sandbox_conf() {
    sed -e s/\"namespace\":.*/\"namespace\":\ \""$1"\",/g "$TESTDATA"/sandbox_config.json >"$TESTDIR/sandbox_config_$1.json"
}

function reload_crio() {
    kill -HUP $CRIO_PID
}

function wait_for_log() {
    CNT=0
    while true; do
        if [[ $CNT -gt 50 ]]; then
            echo wait for log timed out
            exit 1
        fi

        if grep -iq "$1" "$CRIO_LOG"; then
            break
        fi

        echo "waiting for log entry to appear ($CNT): $1"
        sleep 0.1
        CNT=$((CNT + 1))
    done
}

function replace_config() {
    sed -i -e 's;\('"$1"' = "\).*\("\);\1'"$2"'\2;' "$CRIO_CONFIG"
}
