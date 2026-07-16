package checkpointsyncer

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// ErrExternalStorageDisabled means no CheckpointRestoreOperator configures
// externalStorage; the syncer treats this as "be inert", not a hard error.
var ErrExternalStorageDisabled = errors.New("external storage not configured")

// Config is the fully resolved backend configuration + credentials.
type Config struct {
	Backend         string
	Bucket          string
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
}

// ResolveConfig reads the singleton CheckpointRestoreOperator's externalStorage
// block and the referenced Secret (in secretNamespace) into a Config.
func ResolveConfig(ctx context.Context, c client.Client, secretNamespace string) (Config, error) {
	var list criuorgv1.CheckpointRestoreOperatorList
	if err := c.List(ctx, &list); err != nil {
		return Config{}, err
	}
	var es *criuorgv1.ExternalStorageSpec
	for i := range list.Items {
		if list.Items[i].Spec.ExternalStorage != nil {
			es = list.Items[i].Spec.ExternalStorage
			break
		}
	}
	if es == nil {
		return Config{}, ErrExternalStorageDisabled
	}

	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: es.SecretRef.Name}, &sec); err != nil {
		return Config{}, fmt.Errorf("reading credentials secret %q: %w", es.SecretRef.Name, err)
	}
	ak := string(sec.Data["accessKeyID"])
	sk := string(sec.Data["secretAccessKey"])
	if ak == "" || sk == "" {
		return Config{}, fmt.Errorf("secret %q missing accessKeyID/secretAccessKey", es.SecretRef.Name)
	}

	return Config{
		Backend:         es.Backend,
		Bucket:          es.Bucket,
		Endpoint:        es.Endpoint,
		Region:          es.Region,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
	}, nil
}
