#!/bin/bash

wait_for_deployment() {
	local deployment_name="$1"
	timeout=60 # 5 minutes (60 * 5 sec)
	i=1
	echo "Checking if the ${deployment_name} deployment is ready"
	until kubectl -n checkpoint-restore-operator-system get deployment "${deployment_name}" -o jsonpath='{.status.conditions[?(@.status=="True")].type}' | grep "Available" 2>/dev/null; do
		((i++))
		if [[ ${i} -gt ${timeout} ]]; then
			echo "The ${deployment_name} deployment has not become ready before the timeout period"
			echo "Fetching deployment status and describing pods for debugging:"
			kubectl -n checkpoint-restore-operator-system get deployment "${deployment_name}"
			kubectl -n checkpoint-restore-operator-system describe deployment "${deployment_name}"
			kubectl -n checkpoint-restore-operator-system get pods
			kubectl -n checkpoint-restore-operator-system describe pods
			exit 1
		fi
		echo "Waiting for ${deployment_name} deployment to report a ready status"
		sleep 5
	done
	echo "The ${deployment_name} deployment is ready"
}

wait_for_deployment "$1"
