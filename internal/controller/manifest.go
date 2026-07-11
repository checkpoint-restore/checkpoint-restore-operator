package controller

import (
	"time"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"sigs.k8s.io/yaml"
)

const forensicManifestVersion = 1

type forensicManifest struct {
	ManifestVersion int                           `yaml:"manifestVersion"`
	SnapshotChain   forensicManifestSnapshotChain `yaml:"snapshotChain"`
	Entries         []forensicManifestEntry       `yaml:"entries"`
}

type forensicManifestSnapshotChain struct {
	SnapshotChainName string `yaml:"snapshotChainName"`
	Namespace         string `yaml:"namespace"`
	UID               string `yaml:"uid"`
	TargetNamespace   string `yaml:"targetNamespace"`
	SnapshotCount     int32  `yaml:"snapshotCount"`
	StartTime         string `yaml:"startTime,omitempty"`
	CompletionTime    string `yaml:"completionTime,omitempty"`
}

type forensicManifestEntry struct {
	SnapshotIndex  int32  `yaml:"snapshotIndex"`
	PodName        string `yaml:"podName"`
	ContainerName  string `yaml:"containerName"`
	CheckpointPath string `yaml:"checkpointPath"`
	SnapshotTime   string `yaml:"snapshotTime"`
	ArtifactHash   string `yaml:"artifactHash"`
	ParentHash     string `yaml:"parentHash"`
}

func buildManifest(snapshotChain *criuorgv1.ForensicSnapshotChain) ([]byte, error) {
	manifest := forensicManifest{
		ManifestVersion: forensicManifestVersion,
		SnapshotChain: forensicManifestSnapshotChain{
			SnapshotChainName: snapshotChain.Name,
			Namespace:         snapshotChain.Namespace,
			UID:               string(snapshotChain.UID),
			TargetNamespace:   snapshotChain.Spec.Namespace,
			SnapshotCount:     snapshotChain.Status.SnapshotCount,
		},
	}

	if snapshotChain.Status.StartTime != nil {
		manifest.SnapshotChain.StartTime = snapshotChain.Status.StartTime.UTC().Format(time.RFC3339)
	}

	if snapshotChain.Status.CompletionTime != nil {
		manifest.SnapshotChain.CompletionTime = snapshotChain.Status.CompletionTime.UTC().Format(time.RFC3339)
	}

	for _, record := range snapshotChain.Status.SnapshotChainRecords {
		manifest.Entries = append(manifest.Entries, forensicManifestEntry{
			SnapshotIndex:  record.Index,
			PodName:        record.PodName,
			ContainerName:  record.ContainerName,
			CheckpointPath: record.CheckpointPath,
			SnapshotTime:   record.SnapshotTime.UTC().Format(time.RFC3339),
			ArtifactHash:   record.SHA256Hash,
			ParentHash:     record.PreviousSHA256Hash,
		})
	}
	return yaml.Marshal(manifest)
}
