package checkpointsyncer

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

type fakeStore struct {
	mu       sync.Mutex
	put, del []string
	failPut  bool
}

func (f *fakeStore) Put(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut {
		return errFake
	}
	f.put = append(f.put, key)
	return nil
}
func (f *fakeStore) Get(_ context.Context, _, localPath string) error {
	return os.WriteFile(localPath, []byte("archive"), 0o600)
}
func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.del = append(f.del, key)
	return nil
}

var errFake = &os.PathError{Op: "put", Path: "x", Err: os.ErrPermission}

func newReconciler(node string, store ObjectStore) *Reconciler {
	return &Reconciler{
		Client: k8sClient, NodeName: node, SecretNamespace: "default",
		NewStore: func(Config) (ObjectStore, error) { return store, nil },
	}
}

// mustConfigure creates a CRO with externalStorage + creds Secret so
// ResolveConfig succeeds (the fake store ignores the values).
func mustConfigure() {
	_ = k8sClient.Create(ctx, &criuorgv1.CheckpointRestoreOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "default"},
		Spec: criuorgv1.CheckpointRestoreOperatorSpec{
			ExternalStorage: &criuorgv1.ExternalStorageSpec{
				Backend: "s3", Bucket: "b",
				SecretRef: corev1LocalRef("creds"),
			},
		},
	})
	_ = k8sClient.Create(ctx, secretWithCreds("creds", "default"))
}

var _ = Describe("Reconciler", func() {
	It("uploads an archive on its origin node and sets status", func() {
		mustConfigure()
		f := &fakeStore{}
		r := newReconciler("node-a", f)

		tmp, _ := os.CreateTemp(GinkgoT().TempDir(), "*.tar")
		a := &criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ckpt-", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node: "node-a", LocalPath: tmp.Name(),
				Namespace: "default", Pod: "web", Container: "app",
			},
		}
		Expect(k8sClient.Create(ctx, a)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		Expect(err).NotTo(HaveOccurred())

		var got criuorgv1.CheckpointArchive
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, criuorgv1.ConditionArchiveUploaded)).To(BeTrue())
		Expect(got.Status.ExternalURI).To(HavePrefix("s3://b/default/web/app/"))
		Expect(got.Finalizers).To(ContainElement(Finalizer))
		Expect(f.put).To(HaveLen(1))
	})

	It("downloads and stages an archive on a requested non-origin node", func() {
		mustConfigure()
		f := &fakeStore{}
		r := newReconciler("node-b", f) // this pod is on node-b, archive origin is node-a

		dst := filepath.Join(GinkgoT().TempDir(), "checkpoint-web_default-app-y.tar")
		a := &criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ckpt-", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node: "node-a", LocalPath: dst, Namespace: "default", Pod: "web", Container: "app",
				RequestedNodes: []string{"node-b"},
			},
		}
		Expect(k8sClient.Create(ctx, a)).To(Succeed())
		a.Status.ExternalURI = "s3://b/default/web/app/y.tar"
		a.Status.AvailableNodes = []string{"node-a"}
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
			Type: criuorgv1.ConditionArchiveUploaded, Status: metav1.ConditionTrue, Reason: "Uploaded"})
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		Expect(err).NotTo(HaveOccurred())

		Expect(dst).To(BeAnExistingFile()) // fakeStore.Get wrote the file
		var got criuorgv1.CheckpointArchive
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &got)).To(Succeed())
		Expect(got.Status.AvailableNodes).To(ContainElements("node-a", "node-b"))
	})

	It("marks Uploaded=False and does not mark available when upload fails", func() {
		mustConfigure()
		f := &fakeStore{failPut: true}
		r := newReconciler("node-a", f)

		tmp, _ := os.CreateTemp(GinkgoT().TempDir(), "*.tar")
		a := &criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ckpt-", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node: "node-a", LocalPath: tmp.Name(),
				Namespace: "default", Pod: "web", Container: "app",
			},
		}
		Expect(k8sClient.Create(ctx, a)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		Expect(err).To(HaveOccurred())

		var got criuorgv1.CheckpointArchive
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, criuorgv1.ConditionArchiveUploaded)).To(BeFalse())
		Expect(got.Status.AvailableNodes).NotTo(ContainElement("node-a"))
		Expect(f.put).To(BeEmpty())
	})

	It("deletes the object and releases the finalizer on deletion", func() {
		mustConfigure()
		f := &fakeStore{}
		r := newReconciler("node-a", f)
		a := &criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ckpt-", Namespace: "default", Finalizers: []string{Finalizer}},
			Spec:       criuorgv1.CheckpointArchiveSpec{Node: "node-a", LocalPath: "/x.tar", Namespace: "default", Pod: "web", Container: "app"},
		}
		Expect(k8sClient.Create(ctx, a)).To(Succeed())
		a.Status.ExternalURI = "s3://b/default/web/app/x.tar"
		Expect(k8sClient.Status().Update(ctx, a)).To(Succeed())
		Expect(k8sClient.Delete(ctx, a)).To(Succeed()) // finalizer keeps it around

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.del).To(ConsistOf("default/web/app/x.tar"))

		var got criuorgv1.CheckpointArchive
		err = k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &got)
		Expect(client.IgnoreNotFound(err)).To(Succeed()) // finalizer gone ⇒ object deleted
	})
})
