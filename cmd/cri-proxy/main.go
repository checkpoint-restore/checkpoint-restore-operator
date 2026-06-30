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

// Command cri-proxy is the node-side CRI proxy for the PodRestore feature. It
// listens on a unix socket the kubelet is pointed at, forwards every CRI call to
// the real runtime socket, and rewrites CreateContainer to restore from a local
// checkpoint .tar when the Pod sandbox carries the restore annotation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/criproxy"
)

// maxMsgSize matches containerd/CRI-O defaults so large CRI messages (e.g. stats
// or image lists) are not truncated as they pass through the proxy.
const maxMsgSize = 16 * 1024 * 1024

const systemdFirstListenFD = 3

type runtimeVersionClient interface {
	Version(context.Context, *runtimeapi.VersionRequest, ...grpc.CallOption) (*runtimeapi.VersionResponse, error)
}

func checkUpstream(ctx context.Context, client runtimeVersionClient) error {
	_, err := client.Version(ctx, &runtimeapi.VersionRequest{})
	return err
}

func readyzHandler(client runtimeVersionClient, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := checkUpstream(ctx, client); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
}

func systemdActivationFDCount(currentPID int, listenPID, listenFDs string) (int, bool, error) {
	if listenPID == "" && listenFDs == "" {
		return 0, false, nil
	}
	if listenPID == "" || listenFDs == "" {
		return 0, true, fmt.Errorf("LISTEN_PID and LISTEN_FDS must both be set")
	}

	pid, err := strconv.Atoi(listenPID)
	if err != nil {
		return 0, true, fmt.Errorf("parse LISTEN_PID: %w", err)
	}
	if pid != currentPID {
		return 0, false, nil
	}

	fds, err := strconv.Atoi(listenFDs)
	if err != nil {
		return 0, true, fmt.Errorf("parse LISTEN_FDS: %w", err)
	}
	if fds < 0 {
		return 0, true, fmt.Errorf("LISTEN_FDS must not be negative: %d", fds)
	}
	return fds, true, nil
}

func unsetSystemdActivationEnv() {
	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")
}

func inheritedSystemdListener() (net.Listener, bool, error) {
	fds, activated, err := systemdActivationFDCount(
		os.Getpid(),
		os.Getenv("LISTEN_PID"),
		os.Getenv("LISTEN_FDS"),
	)
	if !activated || err != nil {
		return nil, activated, err
	}
	unsetSystemdActivationEnv()

	if fds != 1 {
		return nil, true, fmt.Errorf("expected exactly one systemd socket, got %d", fds)
	}
	file := os.NewFile(uintptr(systemdFirstListenFD), "systemd-listener")
	if file == nil {
		return nil, true, fmt.Errorf("failed to open systemd listener fd %d", systemdFirstListenFD)
	}
	defer func() { _ = file.Close() }()

	lis, err := net.FileListener(file)
	if err != nil {
		return nil, true, fmt.Errorf("use systemd listener fd %d: %w", systemdFirstListenFD, err)
	}
	return lis, true, nil
}

func listenForKubelet(listen string) (net.Listener, bool, error) {
	if lis, inherited, err := inheritedSystemdListener(); inherited || err != nil {
		return lis, inherited, err
	}

	if err := os.MkdirAll(filepath.Dir(listen), 0o700); err != nil {
		return nil, false, fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Remove(listen); err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("remove stale socket: %w", err)
	}

	lis, err := net.Listen("unix", listen)
	if err != nil {
		return nil, false, fmt.Errorf("listen on proxy socket: %w", err)
	}
	if err := os.Chmod(listen, 0o600); err != nil {
		_ = lis.Close()
		_ = os.Remove(listen)
		return nil, false, fmt.Errorf("set proxy socket permissions: %w", err)
	}
	return lis, false, nil
}

func main() {
	var upstream, listen string
	var upstreamTimeout, healthTimeout time.Duration
	var healthAddr string
	flag.StringVar(&upstream, "upstream", "/run/containerd/containerd.sock",
		"path to the real container runtime CRI socket to forward to")
	flag.StringVar(&listen, "listen", "/run/cr-restore-proxy/cri-proxy.sock",
		"path to the unix socket this proxy listens on (point the kubelet here)")
	flag.DurationVar(&upstreamTimeout, "upstream-timeout", 10*time.Second,
		"timeout for the startup check against the upstream runtime")
	flag.StringVar(&healthAddr, "health-bind-address", ":8080",
		"address for /readyz upstream health checks; set empty to disable")
	flag.DurationVar(&healthTimeout, "health-timeout", 2*time.Second,
		"timeout for each /readyz upstream runtime check")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts)).WithName("cri-proxy")
	ctrl.SetLogger(log)

	// Connect to the upstream runtime.
	conn, err := grpc.NewClient(
		"unix:"+upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgSize),
			grpc.MaxCallSendMsgSize(maxMsgSize),
		),
	)
	if err != nil {
		log.Error(err, "failed to connect to upstream runtime", "upstream", upstream)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()
	runtimeClient := runtimeapi.NewRuntimeServiceClient(conn)
	imageClient := runtimeapi.NewImageServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	if err := checkUpstream(ctx, runtimeClient); err != nil {
		cancel()
		log.Error(err, "failed upstream runtime readiness check", "upstream", upstream)
		os.Exit(1)
	}
	cancel()

	lis, inheritedSocket, err := listenForKubelet(listen)
	if err != nil {
		log.Error(err, "failed to prepare kubelet listener", "listen", listen)
		os.Exit(1)
	}
	if inheritedSocket {
		log.Info("using systemd socket activation listener", "listen", listen)
	}

	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMsgSize),
		grpc.MaxSendMsgSize(maxMsgSize),
	)
	runtimeapi.RegisterRuntimeServiceServer(server,
		criproxy.NewRuntimeProxy(runtimeClient, log))
	runtimeapi.RegisterImageServiceServer(server,
		criproxy.NewImageProxy(imageClient, log))

	var healthServer *http.Server
	if healthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/readyz", readyzHandler(runtimeClient, healthTimeout))
		healthServer = &http.Server{
			Addr:              healthAddr,
			Handler:           mux,
			ReadHeaderTimeout: 2 * time.Second,
		}
		go func() {
			if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error(err, "health server failed", "addr", healthAddr)
				server.Stop()
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("shutting down")
		if healthServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = healthServer.Shutdown(ctx)
		}
		server.GracefulStop()
	}()

	log.Info("cri-proxy listening", "listen", listen, "upstream", upstream, "health", healthAddr)
	if err := server.Serve(lis); err != nil {
		log.Error(err, "serve failed")
		if !inheritedSocket {
			_ = os.Remove(listen)
		}
		os.Exit(1)
	}
	if !inheritedSocket {
		_ = os.Remove(listen)
	}
}
