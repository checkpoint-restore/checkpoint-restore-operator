/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package criproxy implements a transparent CRI proxy that sits between the
// kubelet and a container runtime (containerd or CRI-O). It forwards every CRI
// call unchanged, except CreateContainer: when the Pod sandbox carries a
// restore.criu.org/checkpoint-path.<container> annotation for the container being
// created, the proxy rewrites that container's image to the local checkpoint
// .tar path. The runtime then takes its direct checkpoint-restore path (CRIU)
// instead of creating the container from the base image.
//
// This is the node-side mechanism for the PodRestore feature: the kubelet drives
// the normal RunPodSandbox -> CreateContainer -> StartContainer flow and never
// knows a restore is happening; the proxy is the only component that does.
package criproxy

import (
	"context"
	"io"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/checkpointarchive"
)

// Options configures the restore checks the proxy applies before rewriting a
// CreateContainer request. The proxy is the last gate at the node, so it
// validates independently of the PodRestore controller and the admission
// policy — it must not trust the annotation.
type Options struct {
	// CheckpointDir is the only directory restore annotations may reference
	// checkpoint archives in. Empty means criuorgv1.DefaultCheckpointDir;
	// confinement is always enforced so the zero value fails secure.
	CheckpointDir string

	// AllowCrossNamespace disables the check that the namespace recorded
	// inside the checkpoint archive matches the namespace of the pod sandbox
	// being created. Leave false unless deliberate cross-namespace restores
	// are wanted; the operator has a matching --restore-allow-cross-namespace
	// flag that must also be set.
	AllowCrossNamespace bool
}

// RuntimeProxy forwards RuntimeService calls to an upstream runtime, rewriting
// CreateContainer for checkpoint restores.
// Embedding UnimplementedRuntimeServiceServer satisfies the gRPC interface guard
// and ensures any future methods added to the interface return Unimplemented
// instead of failing to compile.
type RuntimeProxy struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	client runtimeapi.RuntimeServiceClient
	opts   Options
	log    logr.Logger
}

// ImageProxy forwards ImageService calls to an upstream runtime unchanged.
// Embedding UnimplementedImageServiceServer satisfies the gRPC interface guard.
type ImageProxy struct {
	runtimeapi.UnimplementedImageServiceServer
	client runtimeapi.ImageServiceClient
	log    logr.Logger
}

// NewRuntimeProxy returns a RuntimeService server that forwards to client,
// applying the restore checks in opts to CreateContainer.
func NewRuntimeProxy(client runtimeapi.RuntimeServiceClient, opts Options, log logr.Logger) *RuntimeProxy {
	return &RuntimeProxy{client: client, opts: opts, log: log}
}

// NewImageProxy returns an ImageService server that forwards to client.
func NewImageProxy(client runtimeapi.ImageServiceClient, log logr.Logger) *ImageProxy {
	return &ImageProxy{client: client, log: log}
}

// rewriteCreateContainer rewrites the container image to the checkpoint archive
// path when the sandbox annotations request a restore for this container. It is
// a no-op (and never panics) when no restore is requested. Returns the path it
// rewrote to, or "" if it left the request unchanged.
//
// When a restore IS requested, the proxy is the last gate before the runtime
// executes a frozen process image, so it fails closed rather than trust the
// annotation. Three checks run, and on any failure an error is returned and
// nothing is forwarded upstream:
//
//  1. The path must have the safe shape the PodRestore controller enforces
//     (absolute, lexically clean, .tar) and live directly inside
//     opts.CheckpointDir — the directory the kubelet checkpoint API writes to —
//     so a forged annotation cannot point the runtime at a .tar staged
//     elsewhere on the node (e.g. through a hostPath or emptyDir mount).
//  2. Unless opts.AllowCrossNamespace, the pod namespace recorded inside the
//     archive by the runtime at checkpoint time must equal the namespace of
//     the sandbox being created. This is the authoritative namespace check
//     (the controller can only inspect the filename); an unreadable archive
//     or one recording no namespace fails closed.
func rewriteCreateContainer(req *runtimeapi.CreateContainerRequest, opts Options, log logr.Logger) (string, error) {
	cfg := req.GetConfig()
	if cfg == nil || cfg.GetMetadata() == nil {
		return "", nil
	}
	name := cfg.GetMetadata().GetName()
	path, ok := req.GetSandboxConfig().GetAnnotations()[criuorgv1.RestoreCheckpointPathAnnotationPrefix+name]
	if !ok || path == "" {
		return "", nil
	}

	if err := criuorgv1.ValidateCheckpointPathInDir(path, opts.CheckpointDir); err != nil {
		log.Error(err, "refusing to restore from an invalid checkpoint path",
			"container", name, "path", path)
		return "", status.Errorf(codes.InvalidArgument,
			"invalid restore checkpoint path for container %q: %v", name, err)
	}

	if !opts.AllowCrossNamespace {
		sandboxNS := req.GetSandboxConfig().GetMetadata().GetNamespace()
		if sandboxNS == "" {
			log.Info("refusing restore: sandbox config carries no namespace",
				"container", name, "path", path)
			return "", status.Errorf(codes.PermissionDenied,
				"restore for container %q denied: pod sandbox records no namespace", name)
		}
		archiveNS, err := checkpointarchive.ReadPodNamespace(path)
		if err != nil {
			log.Error(err, "refusing restore: cannot verify the namespace recorded in the checkpoint",
				"container", name, "path", path)
			return "", status.Errorf(codes.PermissionDenied,
				"restore for container %q denied: cannot verify checkpoint namespace: %v", name, err)
		}
		if archiveNS != sandboxNS {
			log.Info("refusing restore: checkpoint was taken in a different namespace",
				"container", name, "path", path,
				"checkpointNamespace", archiveNS, "sandboxNamespace", sandboxNS)
			return "", status.Errorf(codes.PermissionDenied,
				"restore for container %q denied: checkpoint belongs to namespace %q, sandbox is in %q",
				name, archiveNS, sandboxNS)
		}
	}

	var from string
	img := cfg.GetImage()
	if img != nil {
		from = img.GetImage()
	} else {
		img = &runtimeapi.ImageSpec{}
		cfg.Image = img
	}
	img.Image = path
	log.Info("rewriting container image to checkpoint for restore",
		"container", name, "from", from, "to", path)
	return path, nil
}

// --- RuntimeService: CreateContainer is rewritten; everything else forwards ---

func (p *RuntimeProxy) CreateContainer(ctx context.Context, in *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	if _, err := rewriteCreateContainer(in, p.opts, p.log); err != nil {
		return nil, err
	}
	return p.client.CreateContainer(ctx, in)
}

func (p *RuntimeProxy) Version(ctx context.Context, in *runtimeapi.VersionRequest) (*runtimeapi.VersionResponse, error) {
	return p.client.Version(ctx, in)
}

func (p *RuntimeProxy) RunPodSandbox(ctx context.Context, in *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	return p.client.RunPodSandbox(ctx, in)
}

func (p *RuntimeProxy) StopPodSandbox(ctx context.Context, in *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	return p.client.StopPodSandbox(ctx, in)
}

func (p *RuntimeProxy) RemovePodSandbox(ctx context.Context, in *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	return p.client.RemovePodSandbox(ctx, in)
}

func (p *RuntimeProxy) PodSandboxStatus(ctx context.Context, in *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	return p.client.PodSandboxStatus(ctx, in)
}

func (p *RuntimeProxy) ListPodSandbox(ctx context.Context, in *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	return p.client.ListPodSandbox(ctx, in)
}

func (p *RuntimeProxy) StartContainer(ctx context.Context, in *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	return p.client.StartContainer(ctx, in)
}

func (p *RuntimeProxy) StopContainer(ctx context.Context, in *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	return p.client.StopContainer(ctx, in)
}

func (p *RuntimeProxy) RemoveContainer(ctx context.Context, in *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	return p.client.RemoveContainer(ctx, in)
}

func (p *RuntimeProxy) ListContainers(ctx context.Context, in *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	return p.client.ListContainers(ctx, in)
}

func (p *RuntimeProxy) ContainerStatus(ctx context.Context, in *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	return p.client.ContainerStatus(ctx, in)
}

func (p *RuntimeProxy) UpdateContainerResources(ctx context.Context, in *runtimeapi.UpdateContainerResourcesRequest) (*runtimeapi.UpdateContainerResourcesResponse, error) {
	return p.client.UpdateContainerResources(ctx, in)
}

func (p *RuntimeProxy) ReopenContainerLog(ctx context.Context, in *runtimeapi.ReopenContainerLogRequest) (*runtimeapi.ReopenContainerLogResponse, error) {
	return p.client.ReopenContainerLog(ctx, in)
}

func (p *RuntimeProxy) ExecSync(ctx context.Context, in *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	return p.client.ExecSync(ctx, in)
}

func (p *RuntimeProxy) Exec(ctx context.Context, in *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	return p.client.Exec(ctx, in)
}

func (p *RuntimeProxy) Attach(ctx context.Context, in *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	return p.client.Attach(ctx, in)
}

func (p *RuntimeProxy) PortForward(ctx context.Context, in *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	return p.client.PortForward(ctx, in)
}

func (p *RuntimeProxy) ContainerStats(ctx context.Context, in *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	return p.client.ContainerStats(ctx, in)
}

func (p *RuntimeProxy) ListContainerStats(ctx context.Context, in *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	return p.client.ListContainerStats(ctx, in)
}

func (p *RuntimeProxy) PodSandboxStats(ctx context.Context, in *runtimeapi.PodSandboxStatsRequest) (*runtimeapi.PodSandboxStatsResponse, error) {
	return p.client.PodSandboxStats(ctx, in)
}

func (p *RuntimeProxy) ListPodSandboxStats(ctx context.Context, in *runtimeapi.ListPodSandboxStatsRequest) (*runtimeapi.ListPodSandboxStatsResponse, error) {
	return p.client.ListPodSandboxStats(ctx, in)
}

func (p *RuntimeProxy) UpdateRuntimeConfig(ctx context.Context, in *runtimeapi.UpdateRuntimeConfigRequest) (*runtimeapi.UpdateRuntimeConfigResponse, error) {
	return p.client.UpdateRuntimeConfig(ctx, in)
}

func (p *RuntimeProxy) Status(ctx context.Context, in *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	return p.client.Status(ctx, in)
}

func (p *RuntimeProxy) CheckpointContainer(ctx context.Context, in *runtimeapi.CheckpointContainerRequest) (*runtimeapi.CheckpointContainerResponse, error) {
	return p.client.CheckpointContainer(ctx, in)
}

func (p *RuntimeProxy) ListMetricDescriptors(ctx context.Context, in *runtimeapi.ListMetricDescriptorsRequest) (*runtimeapi.ListMetricDescriptorsResponse, error) {
	return p.client.ListMetricDescriptors(ctx, in)
}

func (p *RuntimeProxy) ListPodSandboxMetrics(ctx context.Context, in *runtimeapi.ListPodSandboxMetricsRequest) (*runtimeapi.ListPodSandboxMetricsResponse, error) {
	return p.client.ListPodSandboxMetrics(ctx, in)
}

func (p *RuntimeProxy) RuntimeConfig(ctx context.Context, in *runtimeapi.RuntimeConfigRequest) (*runtimeapi.RuntimeConfigResponse, error) {
	return p.client.RuntimeConfig(ctx, in)
}

func (p *RuntimeProxy) UpdatePodSandboxResources(ctx context.Context, in *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	return p.client.UpdatePodSandboxResources(ctx, in)
}

// StreamContainers relays a server-streaming container list from the upstream runtime.
func (p *RuntimeProxy) StreamContainers(in *runtimeapi.StreamContainersRequest, srv runtimeapi.RuntimeService_StreamContainersServer) error {
	upstream, err := p.client.StreamContainers(srv.Context(), in)
	if err != nil {
		return err
	}
	for {
		item, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := srv.Send(item); err != nil {
			return err
		}
	}
}

// StreamContainerStats relays a server-streaming stats stream from the upstream runtime.
func (p *RuntimeProxy) StreamContainerStats(in *runtimeapi.StreamContainerStatsRequest, srv runtimeapi.RuntimeService_StreamContainerStatsServer) error {
	upstream, err := p.client.StreamContainerStats(srv.Context(), in)
	if err != nil {
		return err
	}
	for {
		ev, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := srv.Send(ev); err != nil {
			return err
		}
	}
}

// GetContainerEvents relays a server-streaming event stream from the upstream runtime.
func (p *RuntimeProxy) GetContainerEvents(in *runtimeapi.GetEventsRequest, srv runtimeapi.RuntimeService_GetContainerEventsServer) error {
	upstream, err := p.client.GetContainerEvents(srv.Context(), in)
	if err != nil {
		return err
	}
	for {
		ev, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := srv.Send(ev); err != nil {
			return err
		}
	}
}

// --- ImageService: pure passthrough ---

func (p *ImageProxy) ListImages(ctx context.Context, in *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	return p.client.ListImages(ctx, in)
}

func (p *ImageProxy) ImageStatus(ctx context.Context, in *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	return p.client.ImageStatus(ctx, in)
}

func (p *ImageProxy) PullImage(ctx context.Context, in *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	return p.client.PullImage(ctx, in)
}

func (p *ImageProxy) RemoveImage(ctx context.Context, in *runtimeapi.RemoveImageRequest) (*runtimeapi.RemoveImageResponse, error) {
	return p.client.RemoveImage(ctx, in)
}

func (p *ImageProxy) ImageFsInfo(ctx context.Context, in *runtimeapi.ImageFsInfoRequest) (*runtimeapi.ImageFsInfoResponse, error) {
	return p.client.ImageFsInfo(ctx, in)
}

// StreamImages relays a server-streaming image stream from the upstream runtime.
func (p *ImageProxy) StreamImages(in *runtimeapi.StreamImagesRequest, srv runtimeapi.ImageService_StreamImagesServer) error {
	upstream, err := p.client.StreamImages(srv.Context(), in)
	if err != nil {
		return err
	}
	for {
		img, err := upstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := srv.Send(img); err != nil {
			return err
		}
	}
}
