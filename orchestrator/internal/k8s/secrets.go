package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Client) CreatePullSecret(ctx context.Context, namespace, name, pullSecretJSON string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			".dockerconfigjson": []byte(pullSecretJSON),
		},
	}
	return c.createSecret(ctx, secret)
}

func (c *Client) CreateSSHKeySecret(ctx context.Context, namespace, name, sshPublicKey string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ssh-publickey": []byte(sshPublicKey),
		},
	}
	return c.createSecret(ctx, secret)
}

func (c *Client) CreateRedfishConfigSecret(ctx context.Context, namespace, name, configYAML string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"config.yaml": []byte(configYAML),
		},
	}
	return c.createSecret(ctx, secret)
}

func (c *Client) CreateOpaqueSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	return c.createSecret(ctx, secret)
}

func (c *Client) GetSecretData(ctx context.Context, namespace, name, key string) ([]byte, error) {
	secret, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	data, ok := secret.Data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in secret %s/%s", key, namespace, name)
	}
	return data, nil
}

// UpdateSecretData replaces the value of a single key in an existing secret.
func (c *Client) UpdateSecretData(ctx context.Context, namespace, name, key string, value []byte) error {
	secret, err := c.Clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = value
	if _, err := c.Clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *Client) DeleteSecret(ctx context.Context, namespace, name string) error {
	err := c.Clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *Client) CopySecret(ctx context.Context, srcNamespace, srcName, dstNamespace, dstName string) error {
	src, err := c.Clientset.CoreV1().Secrets(srcNamespace).Get(ctx, srcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting source secret %s/%s: %w", srcNamespace, srcName, err)
	}
	dst := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dstName,
			Namespace: dstNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "oct-shift-on-shift-virt",
			},
		},
		Type: src.Type,
		Data: src.Data,
	}
	return c.createSecret(ctx, dst)
}

func (c *Client) createSecret(ctx context.Context, secret *corev1.Secret) error {
	_, err := c.Clientset.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = c.Clientset.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("updating existing secret %s/%s: %w", secret.Namespace, secret.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating secret %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	return nil
}
