#!/usr/bin/env bats

load helpers

function setup() {
	setup_test
	CONTAINER_MANAGE_NS_LIFECYCLE=true CONTAINER_DROP_INFRA_CTR=true start_crio
}

function teardown() {
	cleanup_test
}

@test "test infra ctr dropped" {
	python -c 'import json,sys;obj=json.load(sys.stdin);obj["linux"]["security_context"]["namespace_options"]["pid"] = 1; json.dump(obj, sys.stdout)' \
	   < "$TESTDATA"/sandbox_config.json > "$TESTDIR"/sandbox_no_infra.json
	pod_id=$(crictl runp "$TESTDIR"/sandbox_no_infra.json)

	output=$(runtime list)
	[[ ! "$output" = *"$pod_id"* ]]
}

@test "test infra ctr not dropped" {
	python -c 'import json,sys;obj=json.load(sys.stdin);obj["linux"]["security_context"]["namespace_options"]["pid"] = 0; json.dump(obj, sys.stdout)' \
	   < "$TESTDATA"/sandbox_config.json > "$TESTDIR"/sandbox_no_infra.json
	pod_id=$(crictl runp "$TESTDIR"/sandbox_no_infra.json)

	output=$(runtime list)
	[[ "$output" = *"$pod_id"* ]]
}
