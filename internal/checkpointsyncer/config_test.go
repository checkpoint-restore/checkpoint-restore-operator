package checkpointsyncer

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := criuorgv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveConfigDisabledWhenUnset(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	_, err := ResolveConfig(context.Background(), c, "cro-system")
	if !errors.Is(err, ErrExternalStorageDisabled) {
		t.Fatalf("want ErrExternalStorageDisabled, got %v", err)
	}
}

func TestResolveConfigReadsCRAndSecret(t *testing.T) {
	cro := &criuorgv1.CheckpointRestoreOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "sample"},
		Spec: criuorgv1.CheckpointRestoreOperatorSpec{
			ExternalStorage: &criuorgv1.ExternalStorageSpec{
				Backend: "s3", Bucket: "b", Endpoint: "http://minio:9000", Region: "us",
				SecretRef: corev1.LocalObjectReference{Name: "creds"},
			},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "cro-system"},
		Data:       map[string][]byte{"accessKeyID": []byte("AK"), "secretAccessKey": []byte("SK")},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cro, sec).Build()

	cfg, err := ResolveConfig(context.Background(), c, "cro-system")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "b" || cfg.AccessKeyID != "AK" || cfg.SecretAccessKey != "SK" || cfg.Endpoint != "http://minio:9000" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}

func corev1LocalRef(name string) corev1.LocalObjectReference { return corev1.LocalObjectReference{Name: name} }

func secretWithCreds(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"accessKeyID": []byte("AK"), "secretAccessKey": []byte("SK")},
	}
}
