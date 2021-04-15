#!/usr/bin/env bats

# vim:set ft=bash :

load helpers

function setup() {
	setup_test
}

function teardown() {
	cleanup_test
	rm -f /var/lib/cni/networks/$RANDOM_CNI_NETWORK/*
}

@test "ensure correct hostname" {
	start_crio
	run crictl runp "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crictl create "$pod_id" "$TESTDATA"/container_redis.json "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0  ]
	ctr_id="$output"
	run crictl start "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]

	run crictl exec --sync "$ctr_id" sh -c "hostname"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"crictl_host"* ]]
	run crictl exec --sync "$ctr_id" sh -c "echo \$HOSTNAME"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"crictl_host"* ]]
	run crictl exec --sync "$ctr_id" sh -c "cat /etc/hostname"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"crictl_host"* ]]
}

@test "ensure correct hostname for hostnetwork:true" {
	start_crio
	hostnetworkconfig=$(cat "$TESTDATA"/sandbox_config.json | python -c 'import json,sys;obj=json.load(sys.stdin);obj["linux"]["security_context"]["namespace_options"]["network"] = 2; obj["annotations"] = {}; obj["hostname"] = ""; json.dump(obj, sys.stdout)')
	echo "$hostnetworkconfig" > "$TESTDIR"/sandbox_hostnetwork_config.json
	run crictl runp "$TESTDIR"/sandbox_hostnetwork_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crictl create "$pod_id" "$TESTDATA"/container_redis.json "$TESTDIR"/sandbox_hostnetwork_config.json
	echo "$output"
	[ "$status" -eq 0  ]
	ctr_id="$output"
	run crictl start "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]

	run crictl exec --sync "$ctr_id" sh -c "hostname"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$HOSTNAME"* ]]
	run crictl exec --sync "$ctr_id" sh -c "echo \$HOSTNAME"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$HOSTNAME"* ]]
	run crictl exec --sync "$ctr_id" sh -c "cat /etc/hostname"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$HOSTNAME"* ]]
}

@test "Check for valid pod netns CIDR" {
	start_crio
	run crictl runp "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"

	run crictl create "$pod_id" "$TESTDATA"/container_redis.json "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0  ]
	ctr_id="$output"

    run crictl exec --sync $ctr_id ip addr show dev eth0 scope global 2>&1
    echo "$output"
    [ "$status" -eq 0 ]
    [[ "$output" =~ $POD_IPV4_CIDR_START ]]
    [[ "$output" =~ $POD_IPV6_CIDR_START ]]
}

@test "Ensure correct CNI plugin namespace/name/container-id arguments" {
	start_crio "" "prepare_plugin_test_args_network_conf"
	run crictl runp "$TESTDATA"/sandbox_config.json
	[ "$status" -eq 0 ]

	. $TESTDIR/plugin_test_args.out

	[ "$FOUND_CNI_CONTAINERID" != "redhat.test.crio" ]
	[ "$FOUND_CNI_CONTAINERID" != "podsandbox1" ]
	[ "$FOUND_K8S_POD_NAMESPACE" = "redhat.test.crio" ]
	[ "$FOUND_K8S_POD_NAME" = "podsandbox1" ]
}

@test "Connect to pod hostport from the host" {
	start_crio
	run crictl runp "$TESTDATA"/sandbox_config_hostport.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"

	host_ip=$(get_host_ip)

	run crictl create "$pod_id" "$TESTDATA"/container_config_hostport.json "$TESTDATA"/sandbox_config_hostport.json
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crictl start "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run nc -w 5 $host_ip 4888 </dev/null
	echo "$output"
	[ "$output" = "crictl_host" ]
	[ "$status" -eq 0 ]
	run crictl stop "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
}

@test "Clean up network if pod sandbox fails" {
	# TODO FIXME find a way for sandbox setup to fail if manage ns is true
	cp $(which conmon) "$TESTDIR"/conmon
	CONTAINER_MANAGE_NS_LIFECYCLE=false \
		CONTAINER_DROP_INFRA_CTR=false \
		CONMON_BINARY="$TESTDIR"/conmon \
		start_crio "" "prepare_plugin_test_args_network_conf"

	# make conmon non-executable to cause the sandbox setup to fail after
	# networking has been configured
	chmod 0644 $TESTDIR/conmon
	run crictl runp "$TESTDATA"/sandbox_config.json
	chmod 0755 $TESTDIR/conmon
	echo "$output"
	[ "$status" -ne 0 ]

	# ensure that the server cleaned up sandbox networking if the sandbox
	# failed after network setup
	rm -f /var/lib/cni/networks/$RANDOM_CNI_NETWORK/last_reserved_ip*
	num_allocated=$(ls /var/lib/cni/networks/$RANDOM_CNI_NETWORK | grep -v lock | wc -l)
	[[ "${num_allocated}" == "0" ]]
}

@test "Clean up network if pod sandbox fails after plugin success" {
	start_crio "" "prepare_plugin_test_args_network_conf_malformed_result"

	run crictl runp "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -ne 0 ]

	# ensure that the server cleaned up sandbox networking if the sandbox
	# failed during network setup after the CNI plugin itself succeeded
	rm -f /var/lib/cni/networks/$RANDOM_CNI_NETWORK/last_reserved_ip*
	num_allocated=$(ls /var/lib/cni/networks/$RANDOM_CNI_NETWORK | grep -v lock | wc -l)
	[[ "${num_allocated}" == "0" ]]
}

@test "Clean up network if pod sandbox gets killed" {
	start_crio

	CNI_RESULTS_DIR=/var/lib/cni/results
	POD=$(crictl runp "$TESTDATA/sandbox_config.json")

	# CNI result is there
	# shellcheck disable=SC2010
	[[ $(ls $CNI_RESULTS_DIR | grep "$POD") != "" ]]

	# kill the sandbox
	runtime kill "$POD" KILL

	# wait for the pod to be killed
	while crictl inspectp "$POD" | jq -e '.status.state != "SANDBOX_NOTREADY"' > /dev/null; do
		echo Waiting for sandbox to be stopped
	done

	# now remove the sandbox
	crictl rmp "$POD"

	# CNI result is gone
	# shellcheck disable=SC2010
	[[ $(ls $CNI_RESULTS_DIR | grep "$POD") == "" ]]
}
