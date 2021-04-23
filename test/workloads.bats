#!/usr/bin/env bats

load helpers

function setup() {
	export activation="workload.openshift.io/management"
	export prefix="io.openshift.workload.management"
	setup_test
	sboxconfig="$TESTDIR/sbox.json"
	ctrconfig="$TESTDIR/ctr.json"
}

function teardown() {
	cleanup_test
	if [ -n "$tmp_cg" ]; then
		for controller in cpu cpuset devices; do
			cgdelete -g "$controller:$tmp_cg" -r
		done
	fi
}

function create_workload() {
	local cpushares="$1"
	local cpuset="$2"
	cat << EOF > "$CRIO_CONFIG_DIR/01-workload.conf"
[crio.runtime.workloads.management]
activation_annotation = "$activation"
annotation_prefix = "$prefix"
resources = { "cpushares" =  "$cpushares", "cpuset" = "$cpuset" }
EOF
}

function setup_pod_tests() {
	if [[ "$CONTAINER_CGROUP_MANAGER" == "cgroupfs" ]]; then
		command -v cgcreate &> /dev/null
		declare tmp_cg
		tmp_cg=$(tr -cd 'a-f0-9' < /dev/urandom | head -c 10)
		for controller in cpu cpuset devices; do
			mkdir "/sys/fs/cgroup/$controller/$tmp_cg"
		done
	fi
}

function cgroup_path_from_parent() {
	local parent="$1"
	if [[ "$CONTAINER_CGROUP_MANAGER" == "cgroupfs" ]]; then
		echo "$parent"
	fi
	systemctl status "$parent" | awk '/CGroup/ {print $2}'
}

function check_ctr_fields() {
	local ctr_id="$1"
	local cpushares="$2"
	local cpuset="$3"

	config=$(runtime state "$ctr_id" | jq -r .bundle)/config.json

	if [ -z "$cpushares" ]; then
		[[ $(jq -r .linux.resources.cpu.shares < "$config") == 0 ]]
	else
		[[ $(jq .linux.resources.cpu.shares < "$config") == *"$cpushares"* ]]
	fi

	if [ -z "$cpuset" ]; then
		[[ $(jq -r .linux.resources.cpu.cpus < "$config") == null ]]
	else
		[[ $(jq .linux.resources.cpu.cpus < "$config") == *"$cpuset"* ]]
	fi
}

function check_pod_fields() {
	local pod_cgroup_parent="$1"
	local cpushares="$2"
	local cpuset="$3"

	pod_cgroup_path=$(cgroup_path_from_parent "$pod_cgroup_parent")

	cpuset_file="/sys/fs/cgroup/cpuset/$pod_cgroup_path/cpuset.cpus"
	cpushares_file="/sys/fs/cgroup/cpu/$pod_cgroup_path/cpu.shares"

	if [ -z "$cpushares" ]; then
		# this check assumes we're configuring cpushares to something different than the default
		[[ $(cat "$cpushares_file") != "$cpushares" ]]
	else
		[[ $(cat "$cpushares_file") == "$cpushares" ]]
	fi

	if [ -z "$cpuset" ]; then
		[[ $(cat "$cpuset_file") != "$cpuset" ]]
	else
		[[ $(cat "$cpuset_file") == "$cpuset" ]]
	fi
}

@test "test workload gets configured to defaults" {
	shares="200"
	set="0-1"
	create_workload "$shares" "$set"

	start_crio

	jq --arg act "$activation" ' .annotations[$act] = "true"' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	jq --arg act "$activation" ' .annotations[$act] = "true"' \
		"$TESTDATA"/container_sleep.json > "$ctrconfig"

	ctr_id=$(crictl run "$ctrconfig" "$sboxconfig")

	check_ctr_fields "$ctr_id" "$shares" "$set"
}

@test "test workload can override defaults" {
	shares="200"
	set="0-1"
	name=helloctr
	create_workload "$shares" "0-2"

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$act] = "true"
		|   .annotations[$setkey] = $set' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	jq --arg act "$activation" --arg name "$name" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$act] = "true"
		|   .annotations[$setkey] = $set
		|   .metadata.name = $name' \
		"$TESTDATA"/container_sleep.json > "$ctrconfig"

	ctr_id=$(crictl run "$ctrconfig" "$sboxconfig")
	check_ctr_fields "$ctr_id" "$shares" "$set"
}

@test "test workload should not set if not defaulted or specified" {
	shares="200"
	set=""
	name=helloctr
	create_workload "$shares" ""

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$act] = "true"
		|   .annotations[$setkey] = $set' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	jq --arg act "$activation" --arg name "$name" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$act] = "true"
		|   .annotations[$setkey] = $set
		|   .metadata.name = $name' \
		"$TESTDATA"/container_sleep.json > "$ctrconfig"

	ctr_id=$(crictl run "$ctrconfig" "$sboxconfig")
	check_ctr_fields "$ctr_id" "$shares" "$set"
}

@test "test workload should not set if annotation not specified" {
	shares=""
	set=""
	name=helloctr
	create_workload "200" "0-1"

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$setkey] = $set' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	jq --arg act "$activation" --arg name "$name" --arg set "$set" --arg setkey "$prefix.cpuset/$name" \
		'   .annotations[$setkey] = $set
		|   .metadata.name = $name
		|   del(.linux.resources.cpu_shares)' \
		"$TESTDATA"/container_sleep.json > "$ctrconfig"

	ctr_id=$(crictl run "$ctrconfig" "$sboxconfig")
	check_ctr_fields "$ctr_id" "$shares" "$set"
}

@test "test workload configures pod to defaults" {
	if ! setup_pod_tests; then
		skip "cannot run pod level workloads tests"
	fi

	shares="200"
	set="0-1"
	create_workload "$shares" "$set"

	start_crio

	jq --arg act "$activation" --arg cg "$tmp_cg" \
		' .annotations[$act] = "true"
		| .linux.cgroup_parent = $cg' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	pod_cgroup_parent=$(jq -r .linux.cgroup_parent < "$sboxconfig")

	crictl runp "$sboxconfig"

	check_pod_fields "$pod_cgroup_parent" "$shares" "$set"
}

@test "test workload can override pod defaults" {
	if ! setup_pod_tests; then
		skip "cannot run pod level workloads tests"
	fi

	shares="200"
	set="0-1"
	create_workload "$shares" "0-2"

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/POD" --arg cg "$tmp_cg" \
		'   .annotations[$act] = "true"
		|   .linux.cgroup_parent = $cg
		|   .annotations[$setkey] = $set' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	pod_cgroup_parent=$(jq -r .linux.cgroup_parent < "$sboxconfig")

	crictl runp "$sboxconfig"

	check_pod_fields "$pod_cgroup_parent" "$shares" "$set"
}

@test "test workload should not set if not defaulted or specified for pod" {
	if ! setup_pod_tests; then
		skip "cannot run pod level workloads tests"
	fi

	shares="200"
	set=""
	create_workload "$shares" ""

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/POD" --arg cg "$tmp_cg" \
		'   .annotations[$act] = "true"
		|   .linux.cgroup_parent = $cg
		|   .annotations[$setkey] = $set' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	pod_cgroup_parent=$(jq -r .linux.cgroup_parent < "$sboxconfig")

	crictl runp "$sboxconfig"

	check_pod_fields "$pod_cgroup_parent" "$shares" "$set"
}

@test "test workload should not set if annotation not specified for pod" {
	if ! setup_pod_tests; then
		skip "cannot run pod level workloads tests"
	fi

	shares=""
	set=""
	create_workload "200" "0-1"

	start_crio

	jq --arg act "$activation" --arg set "$set" --arg setkey "$prefix.cpuset/POD" --arg cg "$tmp_cg" \
		'   .annotations[$setkey] = $set
		|   .linux.cgroup_parent = $cg' \
		"$TESTDATA"/sandbox_config.json > "$sboxconfig"

	pod_cgroup_parent=$(jq -r .linux.cgroup_parent < "$sboxconfig")

	crictl runp "$sboxconfig"

	check_pod_fields "$pod_cgroup_parent" "$shares" "$set"
}
