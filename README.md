# argocd-kargo-deploy

A toolkit for putting a hardened GitOps platform (Argo CD for sync, Kargo for
promotion) on any Kubernetes cluster you have a kubeconfig for. Security comes
first, and the installer is built to be driven by you together with your coding
agent.

**Status: early.** It can install Argo CD on a cluster, pinned and verified,
but not yet the rest of the platform, and most installation answers are not
applied yet. `akd` is a placeholder name.

## What works today

`akd questions` prints the installation questionnaire: the decisions an
installation needs, each option's security consequence, and the secure default.

```bash
docker compose --profile cli run --rm --build akd questions
```

`akd preflight` inspects the cluster your kubeconfig points at, without
changing it: Kubernetes version, distro, whether the kubeconfig can install,
whether NetworkPolicy is enforced, and whether any platform component is
already there. See the [agent guide](AGENTS.md) for how to run it against a
real cluster.

`akd components` lists the third-party components the platform will install.
Every chart and image is pinned by digest in
[components/components.lock](components/components.lock), and each image was
verified against its upstream signer when the lock was generated.

`akd plan` turns a valid file of answers into a directory of plain manifests
and a content hash. It works offline from the manifests embedded in the
binary. The plan is incomplete for now: most answers do not change the
manifests yet, and the plan says which.

`akd apply` installs the plan's bootstrap stage, Argo CD, in a namespace that
enforces the restricted Pod Security Standard and denies all network traffic
by default. Sign-in is through your identity provider only, the built-in admin
account is off, and the default project allows nothing. It only accepts the hash of a
plan that has not changed since it was made, and it requires the cluster's
context to be named.

`akd verify` audits the live cluster against the security baseline and maps
each check to the guidance it comes from. It reports what is true, including
any part of the baseline the cluster does not meet. After a bootstrap install
all six checks pass; they cover Argo CD, which is all that is installed so
far.

`akd validate` checks a file of answers against the questionnaire. It rejects
missing answers, unknown fields and any option that weakens the security
baseline without a written acknowledgement, and it lists the acknowledged
departures from the secure defaults.

```bash
docker compose --profile cli run --rm -T akd validate --file - < examples/platform.yaml
```

Add `--output json` to either command for the machine-readable form an agent
consumes. [examples/platform.yaml](examples/platform.yaml) shows the file
format.

## Planned flow

| Step | Command | What it does |
|---|---|---|
| 1 | `akd preflight` | Inspects the cluster. |
| 2 | `akd questions` | Prints the questionnaire. |
| 3 | `akd validate` | Checks the answers in `platform.yaml`. |
| 4 | `akd plan` | Renders what would be applied and prints its hash. |
| 5 | `akd apply --plan <hash>` | Applies exactly the plan you approved. |
| 6 | `akd verify` | Audits the cluster against the security baseline. |

## Documentation

- [Architecture](docs/architecture.md): design, supply chain controls, secrets
  handling and the security baseline.
- [Agent guide](AGENTS.md): how an agent should drive the CLI and work on the
  code.

## Development

Everything runs in containers; only Docker is needed on the host.

```bash
docker compose run --rm dev
```

That runs the tests. See the [agent guide](AGENTS.md) for the other commands.
