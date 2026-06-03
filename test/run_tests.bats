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

# operator_logs prints the operator log; OPERATOR_LOG_CMD can override the
# source for local runs outside the cluster.
function operator_logs() {
  if [ -n "$OPERATOR_LOG_CMD" ]; then
    $OPERATOR_LOG_CMD
  else
    kubectl logs -n checkpoint-restore-operator-system deployment/checkpoint-restore-operator-controller-manager --tail=-1
  fi
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

@test "test_max_total_checkpoint_size" {
  log_and_run kubectl apply -f ./test/test_bySize_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/generate_checkpoint_tar.sh large
  [ "$status" -eq 0 ]
  log_and_run ./test/wait_for_checkpoint_reduction.sh 2
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}

@test "test_max_checkpoint_size" {
  log_and_run sed -i '/^  containerPolicies:/,/maxTotalSize: [0-9]*/ s/^/#/' ./test/test_bySize_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_bySize_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/generate_checkpoint_tar.sh large
  [ "$status" -eq 0 ]
  log_and_run ./test/wait_for_checkpoint_reduction.sh 0
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}

@test "test_orphan_retention_policy" {
  log_and_run kubectl apply -f ./test/test_orphan_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]
  log_and_run ./test/generate_checkpoint_tar.sh
  [ "$status" -eq 0 ]
  log_and_run ./test/wait_for_checkpoint_reduction.sh 0
  [ "$status" -eq 0 ]
  log_and_run ls -la "$CHECKPOINT_DIR"
  [ "$status" -eq 0 ]
}

@test "test_checkpointschedule_validation" {
  run kubectl apply -f ./test/test_checkpointschedule_invalid.yaml
  echo "$output" >&2
  [ "$status" -ne 0 ]
  [[ "$output" == *"interval must be at least 1s"* ]]
}

@test "test_checkpointschedule_interval_status" {
  log_and_run kubectl apply -f ./test/test_checkpointschedule_pod.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl wait --for=condition=Ready --timeout=120s pod/schedule-test-pod
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_checkpointschedule.yaml
  [ "$status" -eq 0 ]
  last=""
  for _ in $(seq 1 30); do
    last=$(kubectl get checkpointschedule schedule-test -o jsonpath='{.status.lastCheckpointTime}')
    if [ -n "$last" ]; then
      break
    fi
    sleep 3
  done
  echo "lastCheckpointTime: $last" >&2
  [ -n "$last" ]
}

@test "test_checkpointschedule_annotation" {
  log_and_run kubectl apply -f ./test/test_checkpointschedule_pod.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl wait --for=condition=Ready --timeout=120s pod/schedule-test-pod
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_checkpointschedule.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl annotate pod schedule-test-pod checkpoint.criu.org/trigger=true --overwrite
  [ "$status" -eq 0 ]
  # The annotation is consumed only after a successful checkpoint; on kind
  # the kubelet checkpoint request always fails (no CRIU in the node image)
  # and the trigger deliberately keeps the annotation for a retry. Accept
  # either outcome as proof that the trigger processed the annotation.
  result=""
  for _ in $(seq 1 30); do
    val=$(kubectl get pod schedule-test-pod -o jsonpath='{.metadata.annotations.checkpoint\.criu\.org/trigger}')
    if [ -z "$val" ]; then
      result="annotation consumed"
      break
    fi
    if operator_logs | grep -q "annotation trigger: checkpoint failed"; then
      result="checkpoint attempted"
      break
    fi
    sleep 5
  done
  echo "annotation trigger result: '$result'" >&2
  [ -n "$result" ]
}

@test "test_checkpointschedule_deletion" {
  log_and_run kubectl apply -f ./test/test_checkpointschedule.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl delete checkpointschedule schedule-test --timeout=60s
  [ "$status" -eq 0 ]
  log_and_run kubectl delete pod schedule-test-pod --ignore-not-found=true
  [ "$status" -eq 0 ]
}

@test "test_checkpointschedule_resource_trigger" {
  log_and_run kubectl apply -f ./test/test_checkpointschedule_resource_pod.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl wait --for=condition=Ready --timeout=120s pod/resource-test-pod
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_checkpointschedule_resource.yaml
  [ "$status" -eq 0 ]
  found=""
  for _ in $(seq 1 60); do
    if operator_logs | grep -q "resource trigger: threshold exceeded"; then
      found="yes"
      break
    fi
    sleep 5
  done
  log_and_run kubectl delete -f ./test/test_checkpointschedule_resource.yaml
  log_and_run kubectl delete -f ./test/test_checkpointschedule_resource_pod.yaml
  [ -n "$found" ]
}

@test "test_checkpointschedule_event_trigger" {
  log_and_run kubectl apply -f ./test/test_checkpointschedule_pod.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl wait --for=condition=Ready --timeout=120s pod/schedule-test-pod
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_checkpointschedule_events.yaml
  [ "$status" -eq 0 ]
  node=$(kubectl get pod schedule-test-pod -o jsonpath='{.spec.nodeName}')
  log_and_run kubectl cordon "$node"
  [ "$status" -eq 0 ]
  found=""
  for _ in $(seq 1 12); do
    if operator_logs | grep -q "event trigger: node drain detected"; then
      found="yes"
      break
    fi
    sleep 5
  done
  log_and_run kubectl uncordon "$node"
  log_and_run kubectl delete -f ./test/test_checkpointschedule_events.yaml
  log_and_run kubectl delete -f ./test/test_checkpointschedule_pod.yaml
  [ -n "$found" ]
}
