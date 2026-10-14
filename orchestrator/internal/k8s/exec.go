package k8s

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecInPod runs a command inside a container and returns stdout/stderr.
// Used for graceful HAProxy reloads (config secret update + SIGUSR2) which
// must NOT restart the pod: a restart would lose its cUDN DHCP IP and every
// DNS record pointing at it.
func (c *Client) ExecInPod(ctx context.Context, namespace, pod, container string, cmd []string) (string, string, error) {
	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.RestConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("creating executor for %s/%s: %w", namespace, pod, err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("exec in %s/%s: %w", namespace, pod, err)
	}
	return stdout.String(), stderr.String(), nil
}
