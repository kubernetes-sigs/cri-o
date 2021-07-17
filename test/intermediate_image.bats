#!/usr/bin/env bats

# this test suite tests crio wipe running with combinations of cri-o and
# podman.

load helpers
PODMAN_BINARY=${PODMAN_BINARY:-$(command -v podman || true)}

function setup(){
    setup_test
}

function teardown(){
    cleanup_test
}

@test "checks removal of intermediate images" {
    if [[ -z "$PODMAN_BINARY" ]]; then
        skip "Podman not installed"
    fi
    
    start_crio

    run podman --root $TESTDIR/crio --runroot $TESTDIR/crio build -f $TESTDATA/intermediate_image -t test 
    [ "$status" -eq 0 ]
   
   
    pre_remove_length=$( podman --runroot $TESTDIR/crio --root $TESTDIR/crio images --all| wc -l)

    run crictl rmi localhost/test 
    [ "$status" -eq 0 ]

    post_remove_length=$( podman --runroot $TESTDIR/crio --root $TESTDIR/crio images --all| wc -l)
   
    [[ $(( "$pre_remove_length" - 3)) == "$post_remove_length" ]]
 
    stop_crio
}

@test "checks listing of intermediate images" {
    if [[ -z "$PODMAN_BINARY" ]]; then
        skip "Podman not installed"
    fi
    
    start_crio

    run podman --root $TESTDIR/crio --runroot $TESTDIR/crio build -f $TESTDATA/intermediate_image -t test 
    [ "$status" -eq 0 ]
  
    podman_image_len=$( podman --runroot $TESTDIR/crio --root $TESTDIR/crio images --all| wc -l)

    crio_image_len=$( crictl images| wc -l)

    [[ $(( "$podman_image_len" - 2)) == "$crio_image_len" ]]

    stop_crio
}
