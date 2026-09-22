The `project build` command builds a Crossplane Project into a set of xpkgs. It
builds each embedded function in the project and a Configuration package that
ties everything together. The output of the build is a special `.xpkg` file
containing all the built packages, placed in the project's output directory
(`_output/` by default). The `project push` command can consume packages from
the output file and push them to an OCI registry.

The `build` command constructs the repository for the built Configuration from
`spec.repository` in `crossplane-project.yaml`. Override it for a single build
with `--repository`.

> **Important:** The repository influences the function names used for embedded
> function references in compositions. You must specify the same repository when
> building and pushing a project.

The build reuses the dependency cache populated by `crossplane dependency add`
and `crossplane dependency update-cache`. Override the cache location with
`--cache-dir` or the `CROSSPLANE_XPKG_CACHE` environment variable.

The CLI builds embedded functions onto a runtime base image from a registry, and
caches those image layers on disk under `crossplane/base-images` in your user
cache directory. A build that needs a layer already in the cache reads it
locally rather than downloading it again, so a build takes longer the first time
it needs a given base image. Projects share the cache.

Layer filenames are content digests, so a cached layer never goes stale and the
cache never needs invalidating. Nothing prunes it, though, so it grows as base
images change. Delete the directory to reclaim the space; the next build refills
what it needs.

## Versioned functions

A `CompositionRevision` records each pipeline step's `functionRef`, but not the
code behind it. Rolling a composite resource back to an older revision restores
the older pipeline and then runs whatever function code is installed under those
names today.

Setting `spec.versionedFunctions: true` in `crossplane-project.yaml` closes that
gap. The build hashes each embedded function's source and appends the hash to
the repository the function is pushed to, so each version of a function's source
becomes its own package and, once installed, its own `Function` object:

```
ghcr.io/my-org/my-project_compose-sql-a1b2c3d4e5f6
  -> Function my-org-my-projectcompose-sql-a1b2c3d4e5f6
```

The pipeline steps in the packaged Compositions and Operations are rewritten to
name the version they were built against. Your own files are not touched: they
keep the stable name, which is what `crossplane function generate` writes and
what `crossplane render` resolves against.

The hash covers everything that goes into the function's runtime image,
including the generated models a function reaches through the `model` symlink in
its directory. It does not cover the runtime base image, so a base image change
reuses the same name at a new digest.

A function reference is a DNS label, which is cut at 63 characters. If your
repository path is long enough that the version would be cut off the end, the
build fails rather than naming every version of the function the same thing.

This costs one repository, one `Function` object and one running function pod
per version of each function's source, for as far back as your rollback targets
reach. Nothing removes the old ones: Crossplane's package manager never deletes
a package it installed to satisfy a dependency.

## Examples

Build the project in the current directory:

```shell
crossplane project build
```

Build the project, overriding the repository:

```shell
crossplane project build --repository=xpkg.crossplane.io/my-org/my-project
```

Build the project into a custom output directory:

```shell
crossplane project build -o ./packages
```
