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
    kubectl logs -n checkpoint-restore-operator deployment/checkpoint-restore-operator-controller-manager --tail=-1
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

# mc_exec runs a one-shot minio/mc pod in the checkpoint-restore-operator
# namespace, aliases it against the in-cluster MinIO deployed by
# test/minio.yaml, and executes $1 against that alias. Returns the exit
# status of the pod (0 only if the pod's container completed successfully),
# so callers can use it to check whether an object exists in the bucket
# (e.g. `mc_exec "mc stat local/checkpoints/<key>"`) without needing an `mc`
# binary on the test runner itself.
mc_exec() {
  local cmd=$1
  local pod="mc-check-$$-${RANDOM}"
  kubectl run "$pod" -n checkpoint-restore-operator --restart=Never --image=minio/mc --command -- \
    sh -c "mc alias set local http://minio.checkpoint-restore-operator:9000 minioadmin minioadmin >/dev/null 2>&1; $cmd" >&2
  kubectl wait -n checkpoint-restore-operator --for=jsonpath='{.status.phase}'=Succeeded --timeout=60s "pod/$pod" >/dev/null 2>&1
  local phase
  phase=$(kubectl get pod -n checkpoint-restore-operator "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)
  kubectl logs -n checkpoint-restore-operator "$pod" >&2 2>/dev/null || true
  kubectl delete pod -n checkpoint-restore-operator "$pod" --ignore-not-found=true --wait=false >/dev/null 2>&1
  [ "$phase" = "Succeeded" ]
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

# Prerequisites for this test (see docs/external_storage.md):
#   - kubelet checkpoint RBAC: kube-apiserver-kubelet-client must be granted
#     `create` on `nodes/checkpoint` (docs/external_storage.md#prerequisites).
#     Without it every checkpoint attempt 403s and no CheckpointArchive is
#     ever created, so this test will time out waiting for one.
#   - The checkpoint-syncer DaemonSet must be deployed and running, e.g.:
#       helm upgrade --install checkpoint-restore-operator \
#         ./charts/checkpoint-restore-operator \
#         --set checkpointSyncer.enabled=true
#     (build/load the checkpoint-syncer image into the cluster first).
#   - The operator (controller-manager) running in the cluster must be built
#     from this branch, so it understands spec.externalStorage and creates
#     CheckpointArchive records for opted-in checkpoints.
@test "test_external_storage_upload_and_delete_minio" {
  # --- Deploy MinIO and create the "checkpoints" bucket ---
  log_and_run kubectl apply -f ./test/minio.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl -n checkpoint-restore-operator rollout status deploy/minio --timeout=120s
  [ "$status" -eq 0 ]
  log_and_run kubectl -n checkpoint-restore-operator wait --for=condition=complete job/minio-create-bucket --timeout=120s
  [ "$status" -eq 0 ]

  # --- Configure the operator: point externalStorage at MinIO and opt every
  # checkpoint in (globalPolicy.uploadToExternalStorage: true) ---
  log_and_run kubectl apply -f ./test/test_externalstorage_checkpointrestoreoperator.yaml
  [ "$status" -eq 0 ]

  # --- Drive a checkpoint via the annotation trigger (mirrors
  # test_checkpointschedule_annotation) ---
  log_and_run kubectl apply -f ./test/test_checkpointschedule_pod.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl wait --for=condition=Ready --timeout=120s pod/schedule-test-pod
  [ "$status" -eq 0 ]
  log_and_run kubectl apply -f ./test/test_checkpointschedule.yaml
  [ "$status" -eq 0 ]
  log_and_run kubectl annotate pod schedule-test-pod checkpoint.criu.org/trigger=true --overwrite
  [ "$status" -eq 0 ]

  # --- Wait for a CheckpointArchive with a populated externalURI and an
  # Uploaded=True condition ---
  archive=""
  uri=""
  uploaded=""
  for _ in $(seq 1 36); do
    archive=$(kubectl get checkpointarchives -n default -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "$archive" ]; then
      uri=$(kubectl get checkpointarchive -n default "$archive" -o jsonpath='{.status.externalURI}' 2>/dev/null || true)
      uploaded=$(kubectl get checkpointarchive -n default "$archive" -o jsonpath='{.status.conditions[?(@.type=="Uploaded")].status}' 2>/dev/null || true)
      if [ -n "$uri" ] && [ "$uploaded" = "True" ]; then
        break
      fi
    fi
    sleep 5
  done
  echo "archive: $archive externalURI: $uri uploaded: $uploaded" >&2
  [ -n "$archive" ]
  [[ "$uri" == s3://* ]]
  [ "$uploaded" = "True" ]

  # --- Verify the object actually landed in the "checkpoints" bucket ---
  key="${uri#s3://checkpoints/}"
  log_and_run mc_exec "mc stat 'local/checkpoints/$key'"
  [ "$status" -eq 0 ]

  # --- Delete the CheckpointArchive; the syncer's delete finalizer must
  # remove the object from the bucket before the CR itself disappears ---
  log_and_run kubectl delete checkpointarchive -n default "$archive" --timeout=60s
  [ "$status" -eq 0 ]

  gone=""
  for _ in $(seq 1 24); do
    if ! kubectl get checkpointarchive -n default "$archive" >/dev/null 2>&1; then
      if ! mc_exec "mc stat 'local/checkpoints/$key'" >/dev/null 2>&1; then
        gone="yes"
        break
      fi
    fi
    sleep 5
  done
  echo "object+CR gone: $gone" >&2

  log_and_run kubectl delete checkpointschedule schedule-test -n default --ignore-not-found=true
  log_and_run kubectl delete pod schedule-test-pod -n default --ignore-not-found=true --timeout=60s
  log_and_run kubectl delete -f ./test/minio.yaml --ignore-not-found=true

  [ -n "$gone" ]
}