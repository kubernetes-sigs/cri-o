#!/usr/bin/env bats
# vim: set syntax=sh: set expandtab:

load helpers

function setup() {
    setup_test
    setup_crio
}

function teardown() {
    cleanup_test
}

function start_crio_static() {
    TAG="$1"
    TMPDIR="$TESTDIR/$TAG"
    mkdir -p "$TMPDIR"

    echo "Downloading CRI-O $TAG to $TMPDIR"
    curl -sfL \
        "https://github.com/cri-o/cri-o/releases/download/$TAG/crio-$TAG.tar.gz" |\
        tar xfz - -C "$TMPDIR" --strip-components=1

    pushd "$TMPDIR"
    echo "Installing"
    DESTDIR="$TMPDIR/i"
    mkdir -p "$DESTDIR/usr/local/bin"
    mkdir -p "$DESTDIR/etc/crio"
    mkdir -p "$DESTDIR/usr/local/share/oci-umount/oci-umount.d"
    mkdir -p "$DESTDIR/usr/local/share/man/man5"
    mkdir -p "$DESTDIR/usr/local/share/man/man8"
    mkdir -p "$DESTDIR/share/bash-completion/completions"
    mkdir -p "$DESTDIR/share/fish/completions"
    mkdir -p "$DESTDIR/share/zsh/site-functions"
    mkdir -p "$DESTDIR/usr/local/lib/systemd/system"
    make DESTDIR="$DESTDIR"
    popd

    echo "Starting CRI-O $TAG"
    "$TMPDIR/i/usr/local/bin/crio" \
        -l debug \
        -c "$CRIO_CONFIG" \
        &> >(tee "$CRIO_LOG") &
    CRIO_PID=$!
    wait_until_reachable
}

function containers_running() {
    crictl ps -o json | jq -e '[.containers[] | select(.state == "CONTAINER_RUNNING")] | length == '$1''
}

function run_upgrade_test() {
    # start previous CRI-O version
    start_crio_static $1

    # start the workloads
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox_config.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox1_config.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox2_config.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox3_config.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox_config_privileged.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox_config_sysctl.json"
    crictl run "$TESTDATA/container_redis.json" "$TESTDATA/sandbox_pidnamespacemode_config.json"

    EXPECTED=7
    containers_running $EXPECTED

    # stop CRI-O
    echo "Killing CRI-O pid $CRIO_PID"
    kill $CRIO_PID
    TIMEOUT=60
    while [ -f "$CRIO_SOCKET" ]; do
        echo "Waiting for CRI-O to shutdown"
        if [ "$TIMEOUT" == 0 ]; then
            echo "Timeout while waiting for CRI-O stop"
            exit 1
        fi
        sleep 1
        ((TIMEOUT--))
    done

    # start currently checked-out CRI-O
    start_crio_no_setup

    # verify that the workload still runs
    containers_running $EXPECTED
}

@test "upgrade path from v1.16.x should succeed" {
    run_upgrade_test v1.16.6
}

@test "upgrade path from v1.17.x should succeed" {
    run_upgrade_test v1.17.4
}

# @test "upgrade path from v1.18.x should succeed" {
    # run_upgrade_test v1.18.3
# }
