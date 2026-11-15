# argocd-kargo-deploy

A toolkit for putting a hardened GitOps platform (Argo CD for sync, Kargo for
promotion) on any Kubernetes cluster you have a kubeconfig for. Security comes
first, and the installer is built to be driven by you together with your coding
agent.

**Status: early scaffold.** The design is written down and the CLI skeleton
exists, but it cannot install anything yet. `akd` is a placeholder name.

## What works today

`akd questions` prints the installation questionnaire: the decisions an
installation needs, each option's security consequence, and the secure default.

```bash
docker compose --profile cli run --rm --build akd questions
```

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
