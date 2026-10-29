# robot-ptr

A Kubernetes controller that points the reverse DNS (PTR) record of a
[Hetzner Robot](https://robot.hetzner.com) server at whatever the pod running on
it asks for, and resets it once that pod is gone.

Typical use: a mail server pinned to one of several dedicated servers needs a
matching PTR on the node it currently runs on.

## How it works

The controller reconciles **nodes**. For each node it

1. picks the node's public IPv4 (first public `ExternalIP`, then `InternalIP`,
   or the `<prefix>/ip` node annotation),
2. looks at the Ready, non-terminating pods on the node that carry the
   `<prefix>/ptr` annotation,
3. sets the PTR to the annotation of the oldest such pod, or to the default if
   there is none.

Pod events only enqueue the node the pod is scheduled on, so a pod moving from
node A to B updates both, and a periodic resync corrects drift and missed events.

| Behaviour       | Details                                                                                                                                                                                                |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Default PTR     | `--default-ptr`, overridable per node with `<prefix>/default-ptr`. Empty deletes the entry, which restores Hetzner's `static.….clients.your-server.de` name.                                           |
| Rolling updates | The last PTR is kept for `--reset-delay` (1m) after the pod left before resetting. The start of the delay is stored in the `<prefix>/pod-left-at` node annotation, so it survives controller restarts. |
| Conflicts       | Several pods with different PTRs on one node: the oldest wins, a `ConflictingPTR` warning event is recorded on the node whenever the conflicting requests change.                                      |
| Robot API usage | Each IP is read at most once per `--resync-interval` (15m) and written only when it differs. Rate limit responses back off for 5 minutes.                                                              |
| Memory          | All pods are cached, but only the fields the controller reads: node name, phase, readiness, timestamps and the `<prefix>/ptr` annotation.                                                              |
| Dry run         | `--dry-run` logs the changes it would make without writing to the Robot API. The `<prefix>/pod-left-at` node annotation is still maintained.                                                           |

The default `<prefix>` is `robot-ptr.nilsit.dev` (`--annotation-prefix`).

## Usage

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: smtp
  annotations:
    robot-ptr.nilsit.dev/ptr: mail.example.com
```

Remember that most receivers also check forward-confirmed reverse DNS, so
`mail.example.com` needs an A record pointing at the same IP, and outgoing
traffic has to leave through that IP (e.g. `hostNetwork` or SNAT).

## Deploy

The Helm chart is published with every release to
`oci://ghcr.io/xnilsit/charts/robot-ptr`. It is built on the
[bjw-s-labs common library](https://bjw-s-labs.github.io/helm-charts/docs/common-library/),
so all of its values (`controllers`, `defaultPodOptions`, `service`,
`serviceMonitor`, ...) are available.

1. Create a webservice user in Robot (*Settings → Webservice and app settings*).
2. Create the credentials secret, or set `credentials.user` and
   `credentials.password` to let the chart create it:

   ```sh
   kubectl -n kube-system create secret generic robot-ptr-credentials \
     --from-literal=ROBOT_USER=... --from-literal=ROBOT_PASSWORD=...
   ```

3. Install in dry-run mode, check the logs, then set `config.dryRun=false`:

   ```sh
   helm install robot-ptr oci://ghcr.io/xnilsit/charts/robot-ptr -n kube-system \
     --set credentials.existingSecret=robot-ptr-credentials --set config.dryRun=true
   ```

The `config` values map to the flags below. The chart derives the rest:

- With `controllers.main.replicas` above 1 it passes `--leader-elect`, adds the
  lease Role and switches to a `RollingUpdate` strategy. A single replica runs
  without leader election and uses `Recreate`, so two controllers never overlap.
- The node `patch` permission is only granted when `config.resetDelay` is not
  zero, because only the reset delay writes the `<prefix>/pod-left-at`
  annotation.

## Flags

| Flag                          | Default                           | Description                                   |
| ----------------------------- | --------------------------------- | --------------------------------------------- |
| `--annotation-prefix`         | `robot-ptr.nilsit.dev`            | Prefix of all annotations.                    |
| `--default-ptr`               | `""`                              | PTR when no annotated pod runs on the node.   |
| `--node-selector`             | `""`                              | Label selector for managed nodes.             |
| `--reset-delay`               | `1m`                              | Grace period before resetting to the default. |
| `--resync-interval`           | `15m`                             | Robot API drift correction interval.          |
| `--dry-run`                   | `false`                           | Log changes without applying them.            |
| `--leader-elect`              | `false`                           | Enable leader election.                       |
| `--robot-url`                 | `https://robot-ws.your-server.de` | Robot webservice URL.                         |
| `--metrics-bind-address`      | `:8080`                           | Metrics endpoint.                             |
| `--health-probe-bind-address` | `:8081`                           | Health probe endpoint.                        |

## Development

Tooling is managed with [mise](https://mise.jdx.dev):

```sh
mise install
mise run test
mise run lint
mise run lint-workflows
mise run test-chart   # helm lint, kubeconform and checks of the rendered manifests
mise run run   # dry-run against the current kubeconfig, needs ROBOT_USER/ROBOT_PASSWORD
```

## Releases

Commits follow [Conventional Commits](https://www.conventionalcommits.org).
[release-please](https://github.com/googleapis/release-please) keeps a release
PR open as soon as a `feat`, `fix`, `perf`, `refactor`, `deps`, `revert` or
breaking commit lands. Other types (`ci`, `chore`, `docs`, ...) don't open one,
but are listed in the changelog of the next release. Merging the release PR tags
the release and publishes a signed multi-arch image to
`ghcr.io/xnilsit/robot-ptr` with `X.Y.Z`, `X.Y`, `X` (from 1.0) and `rolling`
tags, and the Helm chart to `oci://ghcr.io/xnilsit/charts/robot-ptr`. Both are
signed with cosign. Every push to `main` also publishes
`ghcr.io/xnilsit/robot-ptr:main`.

## License

[Apache License 2.0](LICENSE)
