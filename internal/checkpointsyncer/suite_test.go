package checkpointsyncer

import (
	"context"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestCheckpointSyncer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "checkpoint-syncer suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.TODO())
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		// envtest binaries (etcd, kube-apiserver) not present; the specs below
		// skip when k8sClient is nil, so integration tests are skipped rather
		// than failing on environments without the control-plane binaries.
		GinkgoWriter.Printf("envtest not available, skipping integration setup: %v\n", err)
		return
	}
	Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	cancel()
	if cfg == nil {
		return
	}
	Expect(testEnv.Stop()).To(Succeed())
})
