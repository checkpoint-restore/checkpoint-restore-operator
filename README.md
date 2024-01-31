# checkpoint-restore-operator

This is a Kubernetes operator which tries to help managing checkpoints.

## Description

Kubernetes 1.25 [introduced the possibility to create stateful checkpoints
of container](https://kubernetes.io/blog/2022/12/05/forensic-container-checkpointing-alpha/).

One of the early questions asked was how to control how many checkpoint archives
will be on the local disk. Having too many checkpoint archives might not be
useful, depending on the use case, but it might lead to a situation where no
more disk space is left due to a high number of checkpoint archives or if the
checkpoint archives are really large.

With the help of this operator it is possible to limit the number of checkpoint
archives per namespace/pod/container. The maximum number of checkpoints per
namespace/pod/container combination is `10` but it can be changed.

### Available Parameters

The operator has the following parameters which can be changed:

* `maxCheckpointsPerContainer`: how many checkpoint archives should be kept
on disk for a certain namespace/pod/container combination. Defaults to `10`
* `checkpointDirectory`: the directory where checkpoint archives are created
and which should be watched for the correct number of checkpoint archives.
Defaults to kubelet's default checkpoint location `/var/lib/kubelet/checkpoints`.

A sample file can be found at `config/samples/_v1_checkpointrestoreoperator.yaml`
for setting these parameters.

## Getting Started

You’ll need a Kubernetes cluster to run against. You can use
[KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run
against a remote cluster.
**Note:** Your controller will automatically use the current context in your
*kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Running on the Cluster

1. Install Instances of Custom Resources:

   ```sh
   make install
   ```

2. Build and push your image to the location specified by `IMG`:

   ```sh
   make docker-build docker-push IMG=<some-registry>/checkpoint-restore-operator:tag
   ```

3. Deploy the controller to the cluster with the image specified by `IMG`:

   ```sh
   make deploy IMG=<some-registry>/checkpoint-restore-operator:tag
   ```

### Uninstall CRDs

To delete the CRDs from the cluster:

```sh
make uninstall
```

### Undeploy Controller

UnDeploy the controller from the cluster:

```sh
make undeploy
```

## Contributing

While bug fixes can first be identified via an "issue", that is not required.
It's ok to just open up a PR with the fix, but make sure you include the same
information you would have included in an issue - like how to reproduce it.

PRs for new features should include some background on what use cases the
new code is trying to address. When possible and when it makes sense, try to
break-up larger PRs into smaller ones - it's easier to review smaller
code changes. But only if those smaller ones make sense as stand-alone PRs.

Regardless of the type of PR, all PRs should include:

* well documented code changes;
* additional testcases: ideally, they should fail w/o your code change applied;
* documentation changes.

Squash your commits into logical pieces of work that might want to be reviewed
separate from the rest of the PRs. Ideally, each commit should implement a
single idea, and the PR branch should pass the tests at every commit. GitHub
makes it easy to review the cumulative effect of many commits; so, when in
doubt, use smaller commits.

PRs that fix issues should include a reference like `Closes #XXXX` in the
commit message so that github will automatically close the referenced issue
when the PR is merged.

Contributors must assert that they are in compliance with the [Developer
Certificate of Origin 1.1](http://developercertificate.org/). This is achieved
by adding a "Signed-off-by" line containing the contributor's name and e-mail
to every commit message. Your signature certifies that you wrote the patch or
otherwise have the right to pass it on as an open-source patch.

### Test It Out

1. Install the CRDs into the cluster:

   ```sh
   make install
   ```

2. Run your controller (this will run in the foreground, so switch to a new terminal if you want to leave it running):

   ```sh
   make run
   ```

**NOTE:** You can also run this in one step by running: `make install run`

### Modifying the API Definitions

If you are editing the API definitions, generate the manifests such as CRs or CRDs using:

```sh
make manifests
```

**NOTE:** Run `make --help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License and Copyright

Unless mentioned otherwise in a specific file's header, all code in
this project is released under the Apache 2.0 license.

The author of a change remains the copyright holder of their code
(no copyright assignment). The list of authors and contributors can be
retrieved from the git commit history and in some cases, the file headers.
