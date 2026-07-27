package controller

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// Test Key generation function
// We generate a fresh GPG key for each test suite run.
// We never commit a real private key to the repo.
// Returns
// keyBytes : the serialized private key, ready to store in a K8s Secret
// entity : the in-memory entity, used to verify signatures in tests
func newTestSigningKey() (keyBytes []byte, entity *openpgp.Entity) {
	var err error
	entity, err = openpgp.NewEntity(
		"fsc-test",         //test name
		"test key",         //test description
		"test@example.com", //test email
		&packet.Config{
			DefaultHash: crypto.SHA256,
		},
	)
	Expect(err).NotTo(HaveOccurred(), "Failed to generate test signing key")
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	Expect(err).NotTo(HaveOccurred(), "Failed to serialize test signing key")
	Expect(entity.SerializePrivate(w, nil)).To(Succeed())
	Expect(w.Close()).To(Succeed())
	return buf.Bytes(), entity
}

// makeSigningReconciler builds a minimal reconciler with:
// - a fake K8s client containing the given Secret
// - SigningSecretNamespace set to "operator-ns"
// This is enough to call r.signManifest() without a real cluster.
func makeSigningReconciler(secret *corev1.Secret) *ForensicSnapshotChainReconciler {
	Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())

	c := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(secret).
		Build()

	return &ForensicSnapshotChainReconciler{
		Client:                 c,
		SigningSecretNamespace: "operator-ns",
	}
}

// makeSigningSecret builds a corev1.Secret containing the given key bytes
// under the specified data key, in the "operator-ns" namespace.
func makeSigningSecret(name, dataKey string, keyBytes []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "operator-ns",
		},
		Data: map[string][]byte{
			dataKey: keyBytes,
		},
	}
}

var _ = Describe("signingEnabled", func() {
	It("returns false when Signing is nil", func() {
		chain := &criuorgv1.ForensicSnapshotChain{
			Spec: criuorgv1.ForensicSnapshotChainSpec{
				Integrity: criuorgv1.IntegritySpec{
					ForensicSignature: nil, // no signing block in spec
				},
			},
		}
		Expect(signingEnabled(chain)).To(BeFalse())
	})

	It("returns false when Signing.Enabled is false", func() {
		chain := &criuorgv1.ForensicSnapshotChain{
			Spec: criuorgv1.ForensicSnapshotChainSpec{
				Integrity: criuorgv1.IntegritySpec{
					ForensicSignature: &criuorgv1.ForensicSignatureSpec{
						Enabled: false,
					},
				},
			},
		}
		Expect(signingEnabled(chain)).To(BeFalse())
	})

	It("returns true when Signing.Enabled is true", func() {
		chain := &criuorgv1.ForensicSnapshotChain{
			Spec: criuorgv1.ForensicSnapshotChainSpec{
				Integrity: criuorgv1.IntegritySpec{
					ForensicSignature: &criuorgv1.ForensicSignatureSpec{
						Enabled: true,
					},
				},
			},
		}
		Expect(signingEnabled(chain)).To(BeTrue())
	})
})

var _ = Describe("signingSecretRef", func() {
	// signingSecretRef resolves which Secret name and data key to use.
	// Tests verify defaults and user overrides.

	It("returns defaults when spec fields are empty", func() {
		spec := &criuorgv1.ForensicSignatureSpec{} // no SecretRef, no SecretKey

		name, key := signingSecretRef(spec)

		Expect(name).To(Equal(defaultSigningSecretName)) // "forensic-signing-key"
		Expect(key).To(Equal(defaultSigningSecretKey))   // "signing.key"
	})

	It("uses SecretRef.Name and SecretKey when specified", func() {
		spec := &criuorgv1.ForensicSignatureSpec{
			SecretRef: &corev1.LocalObjectReference{Name: "secret-name"},
			SecretKey: "secret-key",
		}

		name, key := signingSecretRef(spec)

		Expect(name).To(Equal("secret-name"))
		Expect(key).To(Equal("secret-key"))
	})
})

var _ = Describe("signManifest", func() {

	// signManifest signs a manifest using the given signing Secret.
	// It verifies:
	// - the Secret exists and has the correct key
	// - the manifest is signed correctly
	// - the signature is added to the manifest
	// - the signature is verified
	// - the signature is added to the manifest
	manifest := []byte("manifest_version: 1\nchain:\n  name: test\nentries: []\n")

	//Sign and Verify a simple manifest
	//Generate a test signing key and store it in a fake K8s Secret
	//Call signManifest, loads the secret and sign the manifest
	//Decode the signature and verify it using the public key
	//If this passes, gpg --verify will also pass
	It("returns a verifiable detached GPG signature", func() {
		keyBytes, entity := newTestSigningKey()

		secret := makeSigningSecret(
			defaultSigningSecretName,
			defaultSigningSecretKey,
			keyBytes,
		)
		r := makeSigningReconciler(secret)

		spec := &criuorgv1.ForensicSignatureSpec{Enabled: true}

		signatureB64, keyID, err := r.signManifest(context.Background(), spec, manifest)

		Expect(err).NotTo(HaveOccurred())

		Expect(signatureB64).NotTo(BeEmpty())
		Expect(keyID).NotTo(BeEmpty())

		signatureBytes, err := base64.StdEncoding.DecodeString(signatureB64)
		Expect(err).NotTo(HaveOccurred())

		//Verify the signature using the public key
		//openpgp.CheckDetachedSignature is the Go equivalent of gpg --verify.
		//It returns the signing entity if verification succeeds, or an error.
		keyRing := openpgp.EntityList{entity}
		_, verifyError := openpgp.CheckDetachedSignature(
			keyRing,
			bytes.NewReader(manifest),
			bytes.NewReader(signatureBytes),
			nil,
		)
		Expect(verifyError).NotTo(HaveOccurred(), "Signature verification failed")
	})

	//Tampered manifest should fail verification
	//A detached signature only verifies against the exact bytes that were
	//signed. If the manifest is modified after signing, verification must
	//fail. This proves the integrity guarantee.
	It("fails verification when manifest is tampered", func() {
		keyBytes, entity := newTestSigningKey()

		secret := makeSigningSecret(
			defaultSigningSecretName,
			defaultSigningSecretKey,
			keyBytes,
		)
		r := makeSigningReconciler(secret)

		spec := &criuorgv1.ForensicSignatureSpec{Enabled: true}

		signatureB64, _, err := r.signManifest(context.Background(), spec, manifest)
		Expect(err).NotTo(HaveOccurred())

		signatureBytes, _ := base64.StdEncoding.DecodeString(signatureB64)

		//Tamper the manifest, say change one byte
		tamperedManifest := []byte("manifest_version: 1\nchain:\n  name: TAMPERED\nentries: []\n")

		keyRing := openpgp.EntityList{entity}
		_, verifyError := openpgp.CheckDetachedSignature(
			keyRing,
			bytes.NewReader(tamperedManifest),
			bytes.NewReader(signatureBytes), //use the original signature
			nil,
		)
		//Verification must fail because the manifest was tampered
		Expect(verifyError).To(HaveOccurred(), "Tampered manifest should fail verification")
	})

	//Missing signing Secret should fail
	// If the Secret doesn't exist in the namespace, signManifest should
	// return an error that includes "load signing secret", as specified
	// in the error message.
	It("errors when the signing secret does not exist", func() {
		//Build reconciler without any secrets
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		r := &ForensicSnapshotChainReconciler{
			Client:                 c,
			SigningSecretNamespace: "operator-ns",
		}

		spec := &criuorgv1.ForensicSignatureSpec{Enabled: true}

		_, _, err := r.signManifest(context.Background(), spec, manifest)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("load signing secret"))
	})

	//Incorrect Secret key should fail
	//The Secret exists but doesn't have the expected key signing.key.
	// signManifest should return an error about the missing data key.
	It("errors when the signing secret has the wrong key", func() {
		keyBytes, _ := newTestSigningKey()

		//Create a secret with "wrong-key" instead of "signing.key"
		secret := makeSigningSecret(
			defaultSigningSecretName,
			"wrong-key",
			keyBytes,
		)
		r := makeSigningReconciler(secret)

		spec := &criuorgv1.ForensicSignatureSpec{Enabled: true}

		_, _, err := r.signManifest(context.Background(), spec, manifest)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("signing-key"))
	})

	//Custom Secret name and key should work
	//Users can override the default Secret name and key using the Spec.
	//This tests that the override is respected and the signature is verified.
	It("uses custom Secret name and key when specified", func() {
		keyBytes, entity := newTestSigningKey()

		secret := makeSigningSecret(
			"custom-secret",
			"custom-key",
			keyBytes,
		)
		r := makeSigningReconciler(secret)

		spec := &criuorgv1.ForensicSignatureSpec{
			SecretRef: &corev1.LocalObjectReference{Name: "custom-secret"},
			SecretKey: "custom-key",
		}

		signatureB64, _, err := r.signManifest(context.Background(), spec, manifest)
		Expect(err).NotTo(HaveOccurred())

		//verify it actually works with the custom secret key
		signatureBytes, _ := base64.StdEncoding.DecodeString(signatureB64)
		keyRing := openpgp.EntityList{entity}
		_, verifyError := openpgp.CheckDetachedSignature(
			keyRing,
			bytes.NewReader(manifest),
			bytes.NewReader(signatureBytes),
			nil,
		)
		Expect(verifyError).NotTo(HaveOccurred())
	})
})
