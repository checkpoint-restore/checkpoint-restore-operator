# checkpoint-restore-operator Helm chart

This chart installs the checkpoint-restore-operator manager, RBAC, metrics
service, restore annotation admission policy, and generated CRDs.

```sh
helm install checkpoint-restore-operator ./charts/checkpoint-restore-operator \
  --namespace checkpoint-restore-operator \
  --create-namespace \
  --set image.tag=latest
```

Released charts are published as OCI artifacts to Quay:

```sh
helm install checkpoint-restore-operator \
  oci://quay.io/criu/charts/checkpoint-restore-operator \
  --version 0.1.0 \
  --namespace checkpoint-restore-operator \
  --create-namespace
```

The CRDs are installed from the chart `crds/` directory. Helm installs these on
first install but does not upgrade or delete CRDs on chart upgrade/uninstall.

All configuration options are documented in [values.yaml](values.yaml) and can
be listed without cloning the repository:

```sh
helm show values oci://quay.io/criu/charts/checkpoint-restore-operator
```

The optional CRI proxy DaemonSet is disabled by default:

```sh
helm install checkpoint-restore-operator ./charts/checkpoint-restore-operator \
  --namespace checkpoint-restore-operator \
  --create-namespace \
  --set image.tag=latest \
  --set criProxy.enabled=true \
  --set criProxy.image.tag=latest
```

The CRI proxy sits in the kubelet-to-runtime path. Prefer the upstream systemd
deployment for reboot-safe production installs, and use the DaemonSet only for
development or canary testing.

Release versions come from the `VERSION` file at the repository root. Merging
a pull request that sets `VERSION` to `X.Y.Z` packages the chart as version
`X.Y.Z`, sets chart `appVersion` to `X.Y.Z`, publishes manager and CRI proxy
images tagged `vX.Y.Z`, creates the `vX.Y.Z` git tag, and creates a GitHub
release with generated notes and the packaged chart attached. The `0.0.0`
placeholder and versions that already have a release are never published.
