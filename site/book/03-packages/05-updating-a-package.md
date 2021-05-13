An independent package records the exact commit where the local fork and the upstream package
diverged. This enables kpt to fetch any update to the upstream package and merge it with local
changes.

## Commit your local changes

Before you update the package, you want to commit your local changes.

First, to see the changes you've made to the fork of the upstream package:

```shell
$ git diff
```

If you're happy with the changes, commit them:

```shell
$ git add .; git commit -m "My changes"
```

## Update the package

For example, you can update to version `v0.2` of the `wordpress` package:

```shell
$ kpt pkg update wordpress@v0.2
```

This is a porcelain for manually updating the `upstream` section in the `Kptfile` :

```yaml
upstream:
  type: git
  git:
    repo: https://github.com/GoogleContainerTools/kpt
    directory: /package-examples/wordpress
    # Change this from v0.1 to v0.2
    ref: v0.2
  updateStrategy: resource-merge
```

and then running:

```shell
$ kpt pkg update wordpress
```

The `update` command updates the local `wordpress` package and the dependent `mysql` package to the
upstream version `v0.2` by doing a 3-way merge between:

1. Original upstream commit
2. New upstream commit
3. Local (edited) package

Several different strategies are available to handle the merge. By default, the `resource-merge`
strategy is used which performs a structural comparison of the resource using OpenAPI schema.

> Refer to the [command reference][update-doc] for more details.

## Commit the updated resources

Once you have successfully updated the package, commit the changes:

```shell
$ git add .; git commit -m "Updated wordpress to v0.2"
```

[update-doc]: /reference/pkg/update/