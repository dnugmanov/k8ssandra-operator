# Changelog

Changelog for the K8ssandra Operator, new PRs should update the `unreleased` section below with entries describing the changes like:

```markdown
* [CHANGE]
* [FEATURE]
* [ENHANCEMENT]
* [BUGFIX]
* [DOCS]
* [TESTING]
```

When cutting a new release, update the `unreleased` heading to the tag being generated and date, like `## vX.Y.Z - YYYY-MM-DD` and create a new placeholder section for  `unreleased` entries.

## unreleased

* [BUGFIX] [#981](https://github.com/k8ssandra/k8ssandra-operator/issues/981) Fix race condition in K8ssandraTask status update
* [DOCS] [#1019](https://github.com/k8ssandra/k8ssandra-operator/issues/1019) Add PR review guidelines documentation
* [BUGFIX] [#1023](https://github.com/k8ssandra/k8ssandra-operator/issues/1023) Ensure merged serverVersion passed to IsNewMetricsEndpointAvailable()
