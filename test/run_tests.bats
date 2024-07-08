#!/usr/bin/env bats

CHECKPOINT_DIR=${CHECKPOINT_DIR:-/var/lib/kubelet/checkpoints}

function log_and_run() {
  echo "Running: $*" >&2
  "$@"
  status=$?
  echo "Status: $status" >&2
  if [ "$status" -ne 0 ]; then
    echo "Command failed with status $status: $*" >&2
    echo "Output:" >&2
    echo "$output" >&2
  fi
  return $status
}

function teardown() {
  log_and_run sudo rm -rf "${CHECKPOINT_DIR:?}"/*
}

@test "test_garbage_collection" {
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/wait_for_checkpoint_reduction.sh 2
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}

@test "test_max_checkpoints_set_to_0" {
  log_and_run sed -i 's/maxCheckpointsPerContainer: [0-9]*/maxCheckpointsPerContainer: 0/' ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run sed -i '/^  containerPolicies:/,/maxCheckpoints: [0-9]*/ s/^/#/' ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/generate_checkpoint_tar.sh
  [ "$status" -eq 0 ]
  log_and_run sleep 2
  log_and_run ./test/wait_for_checkpoint_reduction.sh 5
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}

@test "test_max_checkpoints_set_to_1" {
  log_and_run sed -i 's/maxCheckpointsPerContainer: [0-9]*/maxCheckpointsPerContainer: 1/' ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_byCount_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/generate_checkpoint_tar.sh
  [ "$status" -eq 0 ]
  log_and_run ./test/wait_for_checkpoint_reduction.sh 1
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}
