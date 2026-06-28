package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func parseSHA256FromLogs(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) >= 2 {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading checksum helper pod logs: %w", err)
	}
	return "", fmt.Errorf("no output from helper pod")
}

func computeChecksum(ctx context.Context, c client.Client, clientSet *kubernetes.Clientset, namespace string, nodeName string, snapshotPath string) (string, error) {

	helperPodName := "checksum-helper-" + fmt.Sprintf("%d", time.Now().UnixNano())
	hostPathType := corev1.HostPathDirectory

	//define helper pod spec
	helperPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      helperPodName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "checkpoints-volume",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/lib/kubelet/checkpoints",
							Type: &hostPathType,
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "checksum-helper",
					Image:   "alpine:latest",
					Command: []string{"sh", "-c", "sha256sum " + snapshotPath},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "checkpoints-volume",
							MountPath: "/var/lib/kubelet/checkpoints",
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}

	//create the helper pod
	err := c.Create(ctx, helperPod)
	if err != nil {
		return "", fmt.Errorf("failed to create helper pod: %w", err)
	}

	//always delete the helper pod once chaincreation is done
	defer c.Delete(ctx, helperPod)

	//wait for the helper pod to complete
	// the 30 is really random, but it's a good enough timeout for the helper pod to complete

	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)

		running := &corev1.Pod{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: helperPodName}, running); err != nil {
			return "", fmt.Errorf("failed to get helper pod: %w", err)
		}

		if running.Status.Phase == corev1.PodSucceeded {
			break
		}

		if running.Status.Phase == corev1.PodFailed {
			return "", fmt.Errorf("helper pod failed: %s", running.Status.Message)
		}

		if i == 29 {
			return "", fmt.Errorf("helper pod timed out")
		}
	}

	//Read pod logs to obtain hash output from previous checkpoint and we use clientSet to get the logs

	req := clientSet.CoreV1().Pods(namespace).GetLogs(helperPodName, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream helper pod logs: %w", err)
	}
	defer stream.Close()

	return parseSHA256FromLogs(stream)
}
