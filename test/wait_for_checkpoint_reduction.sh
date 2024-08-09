#!/bin/bash

set -e

CHECKPOINTS_DIR="/var/lib/kubelet/checkpoints"
EXPECTED_COUNT=${1:-2}
TIMEOUT=60
start_time=$(date +%s)

count_tar_files() {
	find "$CHECKPOINTS_DIR" -maxdepth 1 -name "*.tar" -print | wc -l
}

# Function to print logs of operator pods
print_operator_pod_logs() {
	echo "Fetching logs of operator pods:"
	pod_names=$(kubectl -n checkpoint-restore-operator-system get pods -o jsonpath='{.items[*].metadata.name}')
	if [[ -z "$pod_names" ]]; then
		echo "No operator pods found"
	else
		for pod_name in $pod_names; do
			echo "Logs for pod $pod_name:"
			kubectl -n checkpoint-restore-operator-system logs "$pod_name" || echo "Failed to fetch logs for pod $pod_name"
		done
	fi
}

echo "Waiting for checkpoint tar files to be exactly $EXPECTED_COUNT..."

# Wait for the checkpoint tar files to be exactly equal to EXPECTED_COUNT
while true; do
	current_count=$(count_tar_files)
	echo "Checkpoint tar files count: $current_count (waiting for exactly $EXPECTED_COUNT)"
	if [ "$current_count" -eq "$EXPECTED_COUNT" ]; then
		echo "Checkpoint tar files count is exactly $current_count (== $EXPECTED_COUNT)"
		break
	fi
	current_time=$(date +%s)
	elapsed_time=$((current_time - start_time))
	if [ "$elapsed_time" -ge "$TIMEOUT" ]; then
		echo "Timeout reached: Checkpoint tar files count is still $current_count (should be $EXPECTED_COUNT)"
		echo "Fetching checkpoint files for debugging:"
		ls -l "$CHECKPOINTS_DIR"
		print_operator_pod_logs
		exit 1
	fi
	print_operator_pod_logs
	sleep 5
done

echo "Checkpoint reduction check completed successfully."
