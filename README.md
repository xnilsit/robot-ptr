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

| Behaviour            | Details                                                                                                                                                                    |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Default PTR          | `--default-ptr`, overridable per node with `<prefix>/default-ptr`. Empty deletes the entry, which restores Hetzner's `static.….clients.your-server.de` name.                |
| Rolling updates      | The last PTR is kept for `--reset-delay` (1m) after the pod left before resetting. The delay is held in memory, so a controller restart resets immediately.               |
| Conflicts            | Several pods with different PTRs on one node: the oldest wins, a `ConflictingPTR` warning event is recorded on the node.                                                   |
| Robot API usage      | Each IP is read at most once per `--resync-interval` (15m) and written only when it differs. Rate limit responses back off for 5 minutes.                                 |
| Dry run              | `--dry-run` logs the changes it would make without writing anything.                                                                                                      |

The default `<prefix>` is `robot-ptr.xnilsit.github.io` (`--annotation-prefix`).

## Usage

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: smtp
  annotations:
    robot-ptr.xnilsit.github.io/ptr: mail.example.com
```

Remember that most receivers also check forward-confirmed reverse DNS, so
`mail.example.com` needs an A record pointing at the same IP, and outgoing
traffic has to leave through that IP (e.g. `hostNetwork` or SNAT).

## Deploy

1. Create a webservice user in Robot (*Settings → Webservice and app settings*).
2. Create the credentials secret:

   ```sh
   kubectl -n kube-system create secret generic robot-ptr-credentials \
     --from-literal=ROBOT_USER=... --from-literal=ROBOT_PASSWORD=...
   ```

3. Apply `deploy/`, check the logs, then drop `--dry-run`.

## Flags

| Flag                          | Default                           | Description                                   |
| ----------------------------- | --------------------------------- | --------------------------------------------- |
| `--annotation-prefix`         | `robot-ptr.xnilsit.github.io`     | Prefix of all annotations.                    |
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
mise run run   # dry-run against the current kubeconfig, needs ROBOT_USER/ROBOT_PASSWORD
```

## Releases

Commits follow [Conventional Commits](https://www.conventionalcommits.org).
[release-please](https://github.com/googleapis/release-please) keeps a release PR
open; merging it tags the release and publishes a signed multi-arch image to
`ghcr.io/xnilsit/robot-ptr` with `X.Y.Z`, `X.Y`, `X` (from 1.0) and `rolling`
tags. Every push to `main` also publishes `ghcr.io/xnilsit/robot-ptr:main`.
