#!/usr/bin/env bats

load helpers

function setup() {
	if test -n "$CONTAINER_UID_MAPPINGS"; then
		skip "userNS enabled"
	fi

	newconfig="$TESTDIR/config.json"
}

function teardown() {
	cleanup_test
}

@test "should successfully set the device when allowed" {
	setup_test
	ALLOWED_ANNOTATIONS="io.kubernetes.cri-o.Devices" start_crio
	jq '      .annotations."io.kubernetes.cri-o.Devices" = "/dev/null:/dev/qifoo:rwm"' \
		"$TESTDATA"/sandbox_config.json > "$newconfig"
	pod_id=$(crictl runp "$newconfig")
	ctr_id=$(crictl create "$pod_id" "$TESTDATA"/container_redis.json "$TESTDATA"/sandbox_config.json)
	crictl start "$ctr_id"
	output=$(crictl exec --sync "$ctr_id" sh -c "ls /dev/qifoo")
	[ "$output" == "/dev/qifoo" ]
}

@test "should fail to set annotation when not allowed" {
	#allow no annotations

	setup_test
	ALLOWED_ANNOTATIONS="" start_crio
	jq '      .annotations."io.kubernetes.cri-o.Devices" = "/dev/null:/dev/qifoo:rwm"' \
		"$TESTDATA"/sandbox_config.json > "$newconfig"
	pod_id=$(crictl runp "$newconfig")
	ctr_id=$(crictl create "$pod_id" "$TESTDATA"/container_redis.json "$TESTDATA"/sandbox_config.json)
	crictl start "$ctr_id"
	! crictl exec --sync "$ctr_id" sh -c "ls /dev/qifoo"
}
