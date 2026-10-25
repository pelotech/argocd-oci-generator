# argocd-oci-generator

An [Argo CD ApplicationSet plugin generator](https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Generators-Plugin/)
that turns OCI artifacts into Application parameters. Each repository the set names yields one parameter set per
tag it keeps: by default the newest tag that satisfies the set's version constraint. Only the OCI distribution API is
used: tag lists, manifests and blobs, so any registry works and no vendor API lists anything.

| Parameter | Value |
| --- | --- |
| `name` | the repository as the set names it |
| `registry` | the registry host |
| `repository` | the repository path on that registry |
| `tag` | the kept tag |
| `digest` | the manifest digest |
| `annotations` | the manifest annotations |
| `content` | the named file, read out of the artifact's single tar.gz layer; only with `file` |

One static Go binary on a distroless image, built on oras-go for the registry, the library behind the oras CLI
that publishes the artifacts, and Masterminds semver for the constraints, the grammar Argo CD itself uses. A failed
poll answers 502 and Argo CD keeps the last parameters.

## Registry load

Argo CD calls the generator on every reconcile of the set, not only every `requeueAfterSeconds`. The generator
keeps the registry quiet:

- one authorization per registry and scope, cached by oras-go; a poll asks for one token covering every repository;
- a blob is fetched once per layer digest and file, so a steady poll is two requests per repository, the tag list and the manifest;
- a file larger than 1 MiB fails the poll instead of being read; registry calls carry dial, TLS and response timeouts;
- an answer is reused for `MIN_INTERVAL` seconds, 300 by default, so reconcile storms never reach the registry;
- oras-go retries a 429 or a 5xx with backoff and `Retry-After`; one it cannot clear keeps the last answer and holds a minute.

## Set inputs

```yaml
generators:
  - plugin:
      configMapRef:
        name: argocd-oci-generator
      input:
        parameters:
          repositories:                   # below the REGISTRY prefix, or full references with their host
            - services/payments
            - ghcr.io/example/tools/cli
          tag: 3.x                        # a version constraint or an exact tag; default *
          keep:                           # optional; default the newest one
            count: 3                      # the newest three that match
            days: 90                      # and no older than this
          file: config.json               # optional
          platform: linux/amd64           # the child of an image index the file is read from; this is the default
      requeueAfterSeconds: 1800
template:
  metadata:
    name: 'app-{{ .name | base }}'
  spec:
    source:
      helm:
        valuesObject:
          app:
            config: '{{ .content }}'
```

The set names the repositories because the distribution API lists the tags of a repository, not the repositories
of a registry. A publisher that wants new entries to appear with no change to the set pushes them as tags of one
repository and names only that repository.

### Versions

`tag` is the constraint grammar Argo CD uses for `targetRevision`, or an exact tag:

| `tag` | keeps |
| --- | --- |
| `3.0.6`, `latest`, `3.0` | that tag, when it exists: one manifest request, no tag list |
| `*`, `3`, `3.x`, `3.0.x` | the newest release in the range |
| `^3.0.6`, `~3.0.6` | `>=3.0.6 <4.0.0`, `>=3.0.6 <3.1.0`; Terraform's `~>` is `~` |
| `>=3.0.6 <4`, `>=3.0.6, <3.1`, `!=3.0.7 3.0.x` | every term must hold |

A tag is a version when it reads `v?MAJOR.MINOR.PATCH`, with an optional prerelease; prereleases never satisfy a
constraint. An exact tag that does not exist falls back to the constraint, as Argo CD does.

A tag that resolves to an image index, a multi-platform push, yields the index's digest and annotations; with
`file`, the child manifest of `platform` is read, `linux/amd64` by default.

`keep` returns one parameter set per kept tag, newest first: `count` the newest N that match, `days` only
artifacts whose `org.opencontainers.image.created` annotation is at most that old, both together as an AND.
`days` reads one manifest per candidate and stops at the first older one, so it costs at most N+1 manifest
requests. With `keep`, name the Application with the tag, for example `'{{ .name | base }}-{{ .tag }}'`.

The plugin ConfigMap holds `baseUrl`, the Service URL, and `token`, a key of a Secret labeled
`app.kubernetes.io/part-of: argocd` in the Argo CD namespace.

## Environment

| Variable | Meaning |
| --- | --- |
| `REGISTRY` | the default `host/prefix`, for example `registry.example.com/example/apps`, that repositories named without a host are relative to |
| `REGISTRY_USER`, `REGISTRY_PASSWORD` | the credential of the default registry |
| `CREDENTIALS_FILE` | a docker `config.json`, such as a mounted `kubernetes.io/dockerconfigjson` Secret, with a credential per registry host |
| `PLUGIN_TOKEN` | the bearer token Argo CD presents |
| `SSL_CERT_FILE` | a CA bundle, when the registry's chain is private |
| `MIN_INTERVAL` | seconds an answer is reused, default 300 |
| `PORT` | default 8080 |

## Compared with the built-in OCI generator

Argo CD 3.6 adds an `oci` generator ([argoproj/argo-cd#26055](https://github.com/argoproj/argo-cd/issues/26055)).
It mirrors the Git generator: one artifact at one revision, and `directories` or `files` patterns inside it, so one
artifact carries many applications. This generator is the other shape: many artifacts, one file each, with the
newest tag of each resolved at poll time. Use the built-in one when a bundle holds every application; use this one
when each entry is its own artifact with its own release cadence, or on Argo CD before 3.6.

| | built-in `oci` generator | this generator |
| --- | --- | --- |
| Argo CD | 3.6 and later | 3.0 and later, plugin API |
| entries | directories or files of one artifact | one artifact per named repository |
| revision | fixed in the set | newest tag that matches a pattern |
| parameters | path fields and the parsed file | tag, digest, annotations and the raw file |
| credentials | Argo CD repository credentials | its own Secret |
| runs | in the applicationset-controller | one Deployment |

A repository whose first segment holds a dot, a colon or `localhost` is a full reference on that registry;
any other is below `REGISTRY`. One generator can serve sets on several registries, each host found in
`CREDENTIALS_FILE`; `SSL_CERT_FILE` is one bundle for all of them.

## Contributing

Commits and pull request titles follow Conventional Commits; release-please turns them into the changelog, the
next tag and the published image, and bumps the image pin in `deploy/deployment.yaml`. Renovate keeps the
dependencies, actions and base images current. `prek run --all-files` (or `pre-commit`) runs the hooks CI runs.

## Deploy

`deploy/` holds the Deployment and Service. The consumer sets the namespace, the `argocd-oci-generator`
Secret with the environment above, and the image pin. `examples/` is a complete installation next to Argo CD,
the two Secrets, the plugin ConfigMap, an optional private CA and one set: fill the REPLACE values and
`kubectl apply -k examples`.

```bash
go test ./...
tests/integration/run.sh
```

The unit tests run against go-containerregistry's in-memory registry behind TLS and Basic auth; that library is a
test dependency only.

| Path | Holds |
| --- | --- |
| `cmd/argocd-oci-generator` | the binary: environment, wiring, the listener |
| `internal/generator` | registry polling, version constraints, keep, the caches |
| `internal/server` | the plugin generator HTTP API |
| `deploy` | the Deployment and Service for consumers |
| `tests/integration` | the kind end-to-end test |

The integration test needs kind, docker, oras, htpasswd and openssl. It creates a cluster with an authenticated
TLS registry, pushes an artifact at two tags, installs Argo CD, deploys the generator, and checks
that the set creates one Application with the newest tag, digest, version annotation and file of the artifact,
sourced from that artifact and synced. `KEEP=1` keeps the cluster.
