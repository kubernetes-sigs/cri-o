#!/usr/bin/env bats

load helpers

function setup() {
	setup_test
}

function teardown() {
	cleanup_test
}

function run_pids_limit_test() {
	local set_limit="$1"
	local expected_limit="$2"

	CONTAINER_PIDS_LIMIT="$set_limit" start_crio
	jq '	  .command'='["/bin/sleep", "600"]' \
		"$TESTDATA"/container_config.json > "$TESTDIR"/container_pids_limit.json

	ctr_id=$(crictl run "$TESTDIR"/container_pids_limit.json "$TESTDATA"/sandbox_config.json)

	output=$(crictl exec --sync "$ctr_id" sh -c 'cat /sys/fs/cgroup/pids/pids.max 2>/dev/null || cat /sys/fs/cgroup/pids.max')
	echo "got $output, expecting $expected_limit"
	[[ "$output" == "$expected_limit" ]]
}


@test "pids limit" {
	if ! grep -qEw ^pids /proc/cgroups; then
		skip "pids cgroup controller is not available"
	fi
	run_pids_limit_test "1234" "1234"
}

@test "negative pids limit" {
	if ! grep -qEw ^pids /proc/cgroups; then
		skip "pids cgroup controller is not available"
	fi
	run_pids_limit_test "-1" "max"
}

@test "zero pids limit" {
	if ! grep -qEw ^pids /proc/cgroups; then
		skip "pids cgroup controller is not available"
	fi

	# pids_limit of 0 sets pids.max to the systemd default if using systemd
	default_limit=$(systemctl show -p DefaultTasksMax | awk -F = '{ print $2 }')

	# pids_limit of 0 sets pids.max to that of the caller if using cgroupfs
	if [[ "$CONTAINER_CGROUP_MANAGER" == "cgroupfs" ]]; then
		pids_root=/sys/fs/cgroup/pids
		lookfor=pids
		if is_cgroup_v2; then
	￼		pids_root=/sys/fs/cgroup
			lookfor=""
		fi
		default_limit=$(cat "$pids_root/$(awk -v lookfor="$lookfor" -F : '$2 == lookfor {print $3; exit}' /proc/self/cgroup)/pids.max")
	fi
	run_pids_limit_test "0" "$default_limit"
}

@test "conmon custom cgroup" {
	CONTAINER_CGROUP_MANAGER="systemd" CONTAINER_DROP_INFRA_CTR=false CONTAINER_MANAGE_NS_LIFECYCLE=false CONTAINER_CONMON_CGROUP="customcrioconmon.slice" start_crio

	jq '	  .linux.cgroup_parent = "Burstablecriotest123.slice"' \
		"$TESTDATA"/sandbox_config.json > "$TESTDIR"/sandbox_config_slice.json

	pod_id=$(crictl runp "$TESTDIR"/sandbox_config_slice.json)

	output=$(systemctl status "crio-conmon-$pod_id.scope")
	[[ "$output" == *"customcrioconmon.slice"* ]]
}
