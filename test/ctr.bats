#!/usr/bin/env bats

load helpers

function teardown() {
	cleanup_test
}

@test "ctr remove" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr remove --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod stop --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr lifecycle" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl pod list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr status --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr status --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr status --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr remove --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod stop --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod list
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr logging" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl pod list
	echo "$output"
	[ "$status" -eq 0 ]

	# Create a new container.
	newconfig=$(mktemp --tmpdir crio-config.XXXXXX.json)
	cp "$TESTDATA"/container_config_logging.json "$newconfig"
	sed -i 's|"%shellcommand%"|"echo here is some output \&\& echo and some from stderr >\&2"|' "$newconfig"
	run crioctl ctr create --config "$newconfig" --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr_id"
	echo "$output"
	# Ignore errors on stop.
	run crioctl ctr status --id "$ctr_id"
	[ "$status" -eq 0 ]
	run crioctl ctr remove --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]

	# Check that the output is what we expect.
	logpath="$DEFAULT_LOG_PATH/$pod_id/$ctr_id.log"
	[ -f "$logpath" ]
	echo "$logpath :: $(cat "$logpath")"
	grep -E "^[^\n]+ stdout here is some output$" "$logpath"
	grep -E "^[^\n]+ stderr and some from stderr$" "$logpath"

	run crioctl pod stop --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr logging [tty=true]" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl pod list
	echo "$output"
	[ "$status" -eq 0 ]

	# Create a new container.
	newconfig=$(mktemp --tmpdir crio-config.XXXXXX.json)
	cp "$TESTDATA"/container_config_logging.json "$newconfig"
	sed -i 's|"%shellcommand%"|"echo here is some output"|' "$newconfig"
	sed -i 's|"tty": false,|"tty": true,|' "$newconfig"
	run crioctl ctr create --config "$newconfig" --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr_id"
	echo "$output"
	# Ignore errors on stop.
	run crioctl ctr status --id "$ctr_id"
	[ "$status" -eq 0 ]
	run crioctl ctr remove --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]

	# Check that the output is what we expect.
	logpath="$DEFAULT_LOG_PATH/$pod_id/$ctr_id.log"
	[ -f "$logpath" ]
	echo "$logpath :: $(cat "$logpath")"
	grep -E "^[^\n]+ stdout here is some output$" "$logpath"

	run crioctl pod stop --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

# regression test for #127
@test "ctrs status for a pod" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]

	run crioctl ctr list --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "${output}" != "" ]]

	printf '%s\n' "$output" | while IFS= read -r id
	do
		run crioctl ctr status --id "$id"
		echo "$output"
		[ "$status" -eq 0 ]
	done

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr list filtering" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json --name pod1
	echo "$output"
	[ "$status" -eq 0 ]
	pod1_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod1_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr1_id="$output"
	run crioctl ctr start --id "$ctr1_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json --name pod2
	echo "$output"
	[ "$status" -eq 0 ]
	pod2_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod2_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr2_id="$output"
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json --name pod3
	echo "$output"
	[ "$status" -eq 0 ]
	pod3_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod3_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr3_id="$output"
	run crioctl ctr start --id "$ctr3_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr3_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr list --id "$ctr1_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	run crioctl ctr list --id "${ctr1_id:0:4}" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	run crioctl ctr list --id "$ctr2_id" --pod "$pod2_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr2_id"  ]]
	run crioctl ctr list --id "$ctr2_id" --pod "$pod3_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == "" ]]
	run crioctl ctr list --state created --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr2_id"  ]]
	run crioctl ctr list --state running --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	run crioctl ctr list --state stopped --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr3_id"  ]]
	run crioctl ctr list --pod "$pod1_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	run crioctl ctr list --pod "$pod2_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr2_id"  ]]
	run crioctl ctr list --pod "$pod3_id" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr3_id"  ]]
	run crioctl pod remove --id "$pod1_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod2_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod remove --id "$pod3_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr list label filtering" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id" --name ctr1 --label "a=b" --label "c=d" --label "e=f"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr1_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id" --name ctr2 --label "a=b" --label "c=d"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr2_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id" --name ctr3 --label "a=b"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr3_id="$output"
	run crioctl ctr list --label "tier=backend" --label "a=b" --label "c=d" --label "e=f" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	run crioctl ctr list --label "tier=frontend" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == "" ]]
	run crioctl ctr list --label "a=b" --label "c=d" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	[[ "$output" =~ "$ctr2_id"  ]]
	run crioctl ctr list --label "a=b" --quiet
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" != "" ]]
	[[ "$output" =~ "$ctr1_id"  ]]
	[[ "$output" =~ "$ctr2_id"  ]]
	[[ "$output" =~ "$ctr3_id"  ]]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr metadata in list & status" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_config.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"

	run crioctl ctr list --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	# TODO: expected value should not hard coded here
	[[ "$output" =~ "Name: container1" ]]
	[[ "$output" =~ "Attempt: 1" ]]

	run crioctl ctr status --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	# TODO: expected value should not hard coded here
	[[ "$output" =~ "Name: container1" ]]
	[[ "$output" =~ "Attempt: 1" ]]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr execsync" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr execsync --id "$ctr_id" echo HELLO
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" =~ "HELLO" ]]
	run crioctl ctr execsync --id "$ctr_id" --timeout 1 sleep 10
	echo "$output"
	[[ "$output" =~ "command timed out" ]]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr device add" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis_device.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr execsync --id "$ctr_id" ls /dev/mynull
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" =~ "/dev/mynull" ]]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr execsync failure" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr execsync --id "$ctr_id" doesnotexist
	echo "$output"
	[ "$status" -ne 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr execsync exit code" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr execsync --id "$ctr_id" false
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" =~ "Exit code: 1" ]]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr execsync std{out,err}" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr execsync --id "$ctr_id" "echo hello0 stdout"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$(printf "Stdout:\nhello0 stdout")"* ]]
	run crioctl ctr execsync --id "$ctr_id" "echo hello1 stderr >&2"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$(printf "Stderr:\nhello1 stderr")"* ]]
	run crioctl ctr execsync --id "$ctr_id" "echo hello2 stderr >&2; echo hello3 stdout"
	echo "$output"
	[ "$status" -eq 0 ]
	[[ "$output" == *"$(printf "Stderr:\nhello2 stderr")"* ]]
	[[ "$output" == *"$(printf "Stdout:\nhello3 stdout")"* ]]
	run crioctl pod remove --id "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr stop idempotent" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	run crioctl ctr create --config "$TESTDATA"/container_redis.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]
	ctr_id="$output"
	run crioctl ctr start --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl ctr stop --id "$ctr_id"
	echo "$output"
	[ "$status" -eq 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "ctr caps drop" {
	start_crio
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	capsconfig=$(cat "$TESTDATA"/container_config.json | python -c 'import json,sys;obj=json.load(sys.stdin);obj["linux"]["security_context"]["capabilities"] = {u"add_capabilities": [], u"drop_capabilities": [u"mknod", u"kill", u"sys_chroot", u"setuid", u"setgid"]}; json.dump(obj, sys.stdout)')
	echo "$capsconfig" > "$TESTDIR"/container_config_caps.json
	run crioctl ctr create --config "$TESTDIR"/container_config_caps.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}

@test "run ctr with image with Config.Volumes" {
	start_crio
	run crioctl image pull gcr.io/k8s-testimages/redis:e2e
	echo "$output"
	[ "$status" -eq 0 ]
	run crioctl pod run --config "$TESTDATA"/sandbox_config.json
	echo "$output"
	[ "$status" -eq 0 ]
	pod_id="$output"
	volumesconfig=$(cat "$TESTDATA"/container_redis.json | python -c 'import json,sys;obj=json.load(sys.stdin);obj["image"]["image"] = "gcr.io/k8s-testimages/redis:e2e"; json.dump(obj, sys.stdout)')
	echo "$volumesconfig" > "$TESTDIR"/container_config_volumes.json
	run crioctl ctr create --config "$TESTDIR"/container_config_volumes.json --pod "$pod_id"
	echo "$output"
	[ "$status" -eq 0 ]

	cleanup_ctrs
	cleanup_pods
	stop_crio
}
