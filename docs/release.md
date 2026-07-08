# Release Process

Release versions come from the `VERSION` file at the repository root. To
create a new release, open a pull request that changes `VERSION` to the new
version and merge it once approved. Merging a change that sets `VERSION` to
`X.Y.Z` publishes the manager and CRI proxy images tagged `vX.Y.Z` to Docker
Hub and Quay, packages the Helm chart as version `X.Y.Z` and pushes it to
`oci://quay.io/criu/charts`, and creates the `vX.Y.Z` git tag together with a
GitHub release with generated notes and the packaged chart attached.

The `0.0.0` placeholder and versions that already have a release are never
published, so reverting a version bump publishes nothing.

The pull request validation packages the chart with the version from the
`VERSION` file and verifies that the registry credentials have push access to
every repository the release publishes to, so releases fail at review time
rather than after merge. Pull requests from forks cannot access the
repository secrets and skip the credential check.

## If the release workflow fails

Use "Re-run failed jobs" in the GitHub Actions UI. All steps are safe to
re-run, and the job ordering guarantees that the chart is only published
after the images, and the GitHub release only after both. If the merged
commit itself is broken and artifacts were already published under its
version, do not reuse the version: fix the problem and release the next
patch version.
