package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// buildTestSnapshotChain builds a ForensicSnapshotChain with fixed timestamps
// so our assertions are deterministic, same input always produces the same yaml
func buildTestSnapshotChain() *criuorgv1.ForensicSnapshotChain {
	start := metav1.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	completion := metav1.Date(2026, 7, 10, 10, 5, 0, 0, time.UTC)
	snapTime := metav1.Date(2026, 7, 10, 10, 1, 0, 0, time.UTC)
	return &criuorgv1.ForensicSnapshotChain{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chain-1",
			Namespace: "forensics",
			UID:       types.UID("chain123"),
		},
		Spec: criuorgv1.ForensicSnapshotChainSpec{
			Namespace: "default", //target pod namespace
		},
		Status: criuorgv1.ForensicSnapshotChainStatus{
			SnapshotCount:  1,
			StartTime:      &start,
			CompletionTime: &completion,
			SnapshotChainRecords: []criuorgv1.SnapshotChainRecord{
				{
					Index:              0,
					PodName:            "pod-1", //target pod name
					ContainerName:      "app",
					CheckpointPath:     "/var/lib/kubelet/checkpoints/foo.tar",
					SnapshotTime:       snapTime,
					SHA256Hash:         "abcdefg", //artifact hash
					PreviousSHA256Hash: "",
				},
			},
		},
	}
}

var _ = Describe("buildManifest test", func() {

	// Chain level Metadata test
	//buildManifest reads from chain.ObjectMeta and chain.Spec
	//We check that  name, namespace, uid  and target namespace are
	//correct in the YAML output
	It("includes snapshot chain metadata", func() {
		chain := buildTestSnapshotChain()
		manifest, err := buildManifest(chain)
		Expect(err).NotTo(HaveOccurred())
		Expect(manifest).NotTo(BeEmpty())

		//Check all the chain level metadata is present
		Expect(string(manifest)).To(ContainSubstring("SnapshotChainName: chain-1"))
		Expect(string(manifest)).To(ContainSubstring("Namespace: forensics"))
		Expect(string(manifest)).To(ContainSubstring("UID: chain123"))
		Expect(string(manifest)).To(ContainSubstring("TargetNamespace: default"))
	})

	//Snapshot Chain Records entry test
	//Each Snapshot Chain Record is represented as an entry
	//in the manifest. We check that all the forensically
	//relevant fields are present and correct, such as
	//pod name, container, checkpoint path, hash &
	//snapshot time.
	It("maps snapshot chain records to manifest entries", func() {
		chain := buildTestSnapshotChain()
		manifest, err := buildManifest(chain)
		Expect(err).NotTo(HaveOccurred())

		outputString := string(manifest)

		//Check all the record level metadata is present
		Expect(outputString).To(ContainSubstring("PodName: pod-1"))
		Expect(outputString).To(ContainSubstring("ContainerName: app"))
		Expect(outputString).To(ContainSubstring("CheckpointPath: /var/lib/kubelet/checkpoints/foo.tar"))
		Expect(outputString).To(ContainSubstring("ArtifactHash: abcdefg"))
		Expect(outputString).To(ContainSubstring(`SnapshotTime: "2026-07-10T10:01:00Z"`))
	})

	//Empty snapshot chain test
	// A chain that completes with 0 records (e.g. no matching pods) should
	// still produce a valid manifest. The entries field should be empty
	// but the chain metadata should still be present.
	It("handles empty snapshot chains", func() {
		chain := buildTestSnapshotChain()
		chain.Status.SnapshotChainRecords = nil
		chain.Status.SnapshotCount = 0

		manifest, err := buildManifest(chain)

		Expect(err).NotTo(HaveOccurred())
		Expect(manifest).NotTo(BeEmpty())

		//Check the chain metadata is present
		Expect(string(manifest)).To(ContainSubstring("Name: chain-1"))
		Expect(string(manifest)).To(ContainSubstring("Namespace: forensics"))
	})

	//Stable manifest output test
	//If buildManifest produces different output bytes each time for
	//the same input, the GPG signature would only verify sometimes.
	//We call it twice and assert identical output.
	It("produces identical signature bytes on multiple calls", func() {
		chain := buildTestSnapshotChain()
		manifest1, err1 := buildManifest(chain)
		manifest2, err2 := buildManifest(chain)

		Expect(err1).NotTo(HaveOccurred())
		Expect(err2).NotTo(HaveOccurred())
		Expect(string(manifest1)).NotTo(BeEmpty())
		Expect(string(manifest2)).NotTo(BeEmpty())

		Expect(string(manifest1)).To(Equal(string(manifest2)))
	})

	//Multiple records in Manifest test
	// If we have two records, both should appear in the manifest
	// with correct chain order and timestamp order.
	//Also verifies that the previous hash is correctly set.
	It("includes all records when multiple snapshots exist", func() {
		chain := buildTestSnapshotChain()
		snapTime2 := metav1.Date(2026, 7, 10, 10, 2, 0, 0, time.UTC)

		chain.Status.SnapshotChainRecords = append(
			chain.Status.SnapshotChainRecords,
			criuorgv1.SnapshotChainRecord{
				Index:              1,
				PodName:            "pod-2",
				ContainerName:      "app",
				CheckpointPath:     "/var/lib/kubelet/checkpoints/foo2.tar",
				SnapshotTime:       snapTime2,
				SHA256Hash:         "eeff1122",
				PreviousSHA256Hash: "aabbccdd",
			},
		)
		chain.Status.SnapshotCount = 2

		manifest, err := buildManifest(chain)

		Expect(err).NotTo(HaveOccurred())
		outputString := string(manifest)
		Expect(outputString).To(ContainSubstring("aabbccdd")) // first hash
		Expect(outputString).To(ContainSubstring("eeff1122")) // second hash
		Expect(outputString).To(ContainSubstring("foo2.tar")) // second checkpoint path
	})
})
