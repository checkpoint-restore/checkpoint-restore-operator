package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/ProtonMail/go-crypto/openpgp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

const (
	defaultSigningSecretName = "forensic-signing-key"
	defaultSigningSecretKey  = "signing-key"
)

func signingEnabled(chain *criuorgv1.ForensicSnapshotChain) bool {
	return chain.Spec.Integrity.ForensicSignature != nil && chain.Spec.Integrity.ForensicSignature.Enabled
}

func signingSecretRef(spec *criuorgv1.ForensicSignatureSpec) (name, key string) {
	name, key = defaultSigningSecretName, defaultSigningSecretKey
	if spec.SecretRef != nil && spec.SecretRef.Name != "" {
		name = spec.SecretRef.Name
	}
	if spec.SecretKey != "" {
		key = spec.SecretKey
	}
	return name, key
}

func (r *ForensicSnapshotChainReconciler) signManifest(
	ctx context.Context,
	spec *criuorgv1.ForensicSignatureSpec,
	manifest []byte,
) (sigB64, keyID string, err error) {
	secretName, secretKey := signingSecretRef(spec)

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.SigningSecretNamespace,
		Name:      secretName,
	}, secret); err != nil {
		return "", "", fmt.Errorf("load signing secret: %w", err)
	}

	raw, ok := secret.Data[secretKey]
	if !ok {
		return "", "", fmt.Errorf("secret %q data key %q not found", secretName, secretKey)
	}

	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("failed to read signing key: %w", err)
	}

	entity := ring[0]
	if entity.PrivateKey == nil {
		return "", "", fmt.Errorf("no private key in %q", secretName)
	}

	var sigBuf bytes.Buffer
	if err := openpgp.DetachSign(&sigBuf, entity, bytes.NewReader(manifest), nil); err != nil {
		return "", "", fmt.Errorf("detach-sign manifest: %w", err)
	}

	return base64.StdEncoding.EncodeToString(sigBuf.Bytes()), entity.PrimaryKey.KeyIdString(),
		nil
}
