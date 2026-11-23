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

// Command checkpoint-store-agent runs on every node, uploading the node's
// checkpoint archives to the object storage configured on the
// CheckpointRestoreOperator and downloading stored ones for the PodRestores
// that target the node.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storageconfig"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storeagent"
)

func main() {
	var (
		checkpointDir       string
		stateDir            string
		probeAddr           string
		allowCrossNamespace bool
		interval            time.Duration
		settle              time.Duration
	)
	flag.StringVar(&checkpointDir, "checkpoint-dir", criuorgv1.DefaultCheckpointDir,
		"Directory the kubelet writes checkpoint archives to.")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/checkpoint-store-agent",
		"Directory on the node, outside the checkpoint directory, for the agent's records.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	flag.BoolVar(&allowCrossNamespace, "allow-cross-namespace", false,
		"Allow a PodRestore to restore a snapshot taken in another namespace.")
	flag.DurationVar(&interval, "scan-interval", 15*time.Second, "How often to scan for new checkpoint archives.")
	flag.DurationVar(&settle, "settle-time", 10*time.Second,
		"How long an archive must go unmodified before it is uploaded.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("checkpoint-store-agent")

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Error(nil, "NODE_NAME must be set to the node the agent runs on")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(criuorgv1.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		log.Error(err, "creating the manager")
		os.Exit(1)
	}

	agent := &storeagent.Agent{
		Client:              mgr.GetClient(),
		Storage:             storageconfig.NewProvider(nil),
		NodeName:            nodeName,
		CheckpointDir:       checkpointDir,
		StateDir:            stateDir,
		AllowCrossNamespace: allowCrossNamespace,
		Interval:            interval,
		Settle:              settle,
		Log:                 log,
	}
	if err := agent.SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the download reconciler")
		os.Exit(1)
	}
	if err := mgr.Add(agent); err != nil {
		log.Error(err, "adding the upload loop")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "adding the health check")
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running the agent")
		os.Exit(1)
	}
}
