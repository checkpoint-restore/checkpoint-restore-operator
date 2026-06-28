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

function setup() {
  TEST_TMPDIR=$(mktemp -d)
}

function teardown() {
  log_and_run sudo rm -rf "${CHECKPOINT_DIR:?}"/*
  rm -rf "$TEST_TMPDIR"
}

# Copy a config to a temp location before modifying it with sed so that
# git-tracked test fixtures are never mutated in place.
function copy_config() {
  local src=$1
  local dst="$TEST_TMPDIR/$(basename "$src")"
  cp "$src" "$dst"
  echo "$dst"
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

# Deploy a nginx pod for FSC tests
fsc_deploy_nginx_pod() {
  local pod_name=$1
  log_and_run kubectl run "$pod_name" --image=nginx --restart=Never --labels=app=nginx
  log_and_run kubectl wait --for=condition=Ready --timeout=120s "pod/$pod_name"
}

# Wait until ForensicSnapshotChain reaches expected phase
fsc_wait_for_phase() {
  local name=$1
  local expected=$2
  local attempts=${3:-60}
  local sleep_secs=${4:-5}
  local phase=""
  for _ in $(seq 1 "$attempts"); do
    phase=$(kubectl get forensicsnapshotchain "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [ "$phase" = "$expected" ] && break
    sleep "$sleep_secs"
  done
  echo "fsc $name phase: $phase (expected $expected)" >&2
  [ "$phase" = "$expected" ]
}

# Cleanup FSC CR and pod
fsc_cleanup() {
  local yaml=$1
  local pod=$2
  log_and_run kubectl delete -f "$yaml" --ignore-not-found=true
  log_and_run kubectl delete pod "$pod" --ignore-not-found=true --timeout=60s
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
  config=$(copy_config ./test/test_byCount_checkpointrestoreoperator.yaml)
  log_and_run sed -i 's/maxCheckpointsPerContainer: [0-9]*/maxCheckpointsPerContainer: 0/' "$config"
  [ "$status" -eq 0 ]
  log_and_run sed -i '/^  containerPolicies:/,/maxCheckpoints: [0-9]*/ s/^/#/' "$config"
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f "$config"
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
  config=$(copy_config ./test/test_byCount_checkpointrestoreoperator.yaml)
  log_and_run sed -i 's/maxCheckpointsPerContainer: [0-9]*/maxCheckpointsPerContainer: 1/' "$config"
  [ "$status" -eq 0 ]
  log_and_run sed -i '/^  containerPolicies:/,/maxCheckpoints: [0-9]*/ s/^/#/' "$config"
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f "$config"
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
  config=$(copy_config ./test/test_bySize_checkpointrestoreoperator.yaml)
  log_and_run sed -i '/^  containerPolicies:/,/maxTotalSize: [0-9]*/ s/^/#/' "$config"
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f "$config"
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


@test "test_forensicsnapshotchain_max_snapshots" {
  fsc_deploy_nginx_pod fsc-maxsnap-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_maxsnapshots.yaml
  fsc_wait_for_phase max-snapshots-test Completed

  count=$(kubectl get forensicsnapshotchain max-snapshots-test -o jsonpath='{.status.snapshotCount}')
  echo "snapshotCount: $count" >&2
  [ "$count" -eq 3 ]

  reason=$(kubectl get forensicsnapshotchain max-snapshots-test -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  [ "$reason" = "MaxSnapshotsReached" ]

  fsc_cleanup ./test/test_forensicsnapshotchain_maxsnapshots.yaml fsc-maxsnap-nginx
}

@test "test_forensicsnapshotchain_max_duration" {
  fsc_deploy_nginx_pod fsc-maxdur-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_maxduration.yaml
  fsc_wait_for_phase max-duration-test Completed 30 5

  reason=$(kubectl get forensicsnapshotchain max-duration-test -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  [ "$reason" = "MaxDurationReached" ]

  fsc_cleanup ./test/test_forensicsnapshotchain_maxduration.yaml fsc-maxdur-nginx
}

@test "test_forensicsnapshotchain_default_interval" {
  fsc_deploy_nginx_pod fsc-interval-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_default_interval.yaml
  fsc_wait_for_phase default-interval-test Completed 90 5

  count=$(kubectl get forensicsnapshotchain default-interval-test -o jsonpath='{.status.snapshotCount}')
  [ "$count" -eq 5 ]

  fsc_cleanup ./test/test_forensicsnapshotchain_default_interval.yaml fsc-interval-nginx
}

@test "test_forensicsnapshotchain_post_snapshot_delete_pod" {
  fsc_deploy_nginx_pod fsc-delete-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_postsnapshotaction_deletepod.yaml
  fsc_wait_for_phase max-snapshots-test Completed

  # Pod should be gone after chain completes
  run kubectl get pod fsc-delete-nginx
  [ "$status" -ne 0 ]

  fsc_cleanup ./test/test_forensicsnapshotchain_postsnapshotaction_deletepod.yaml fsc-delete-nginx
}

@test "test_forensicsnapshotchain_integrity" {
  fsc_deploy_nginx_pod integrity-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_integrity.yaml
  fsc_wait_for_phase integrity-test Completed

  count=$(kubectl get forensicsnapshotchain integrity-test -o jsonpath='{.status.snapshotCount}')
  echo "snapshotCount: $count" >&2
  [ "$count" -ge 1 ]

  hash=$(kubectl get forensicsnapshotchain integrity-test -o jsonpath='{.status.snapshotChainRecords[0].sha256Hash}')
  [ -n "$hash" ]
  [ "${#hash}" -eq 64 ]   

  integrity=$(kubectl get forensicsnapshotchain integrity-test -o jsonpath='{.status.conditions[?(@.type=="IntegrityVerified")].status}')
  [ "$integrity" = "True" ]

  if [ "$count" -ge 2 ]; then
    prev=$(kubectl get forensicsnapshotchain integrity-test -o jsonpath='{.status.snapshotChainRecords[1].previousSHA256Hash}')
    first=$(kubectl get forensicsnapshotchain integrity-test -o jsonpath='{.status.snapshotChainRecords[0].sha256Hash}')
    [ "$prev" = "$first" ]
  fi

  fsc_cleanup ./test/test_forensicsnapshotchain_integrity.yaml integrity-nginx
}

@test "test_forensicsnapshotchain_integrity_unsupported_algorithm" {
  fsc_deploy_nginx_pod integrity-invalid-nginx
  log_and_run kubectl apply -f ./test/test_forensicsnapshotchain_integrity_invalid.yaml
  fsc_wait_for_phase integrity-invalid-test Failed 12 5

  msg=$(kubectl get forensicsnapshotchain integrity-invalid-test -o jsonpath='{.status.errorMessage}')
  [[ "$msg" == *"md5"* ]]

  fsc_cleanup ./test/test_forensicsnapshotchain_integrity_invalid.yaml integrity-invalid-nginx
}