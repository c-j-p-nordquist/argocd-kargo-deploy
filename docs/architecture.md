# Architecture

Status: draft, not yet implemented. `akd` is a placeholder name for the CLI.

## Goal

Take a user from "I have a kubeconfig" to a hardened, verified GitOps platform
(Argo CD for sync, Kargo for promotion) on any conformant Kubernetes cluster:
GKE, Talos, k3s or anything else. Security is the first priority, ahead of
convenience and feature count.

The toolkit is a distribution and an installer. It does not reimplement Argo CD
or Kargo, provision clusters, or ship a UI.

## Settled decisions

| Area | Choice | Why |
|---|---|---|
| CLI language | Go | Single static binary, testable, good Kubernetes and sigstore libraries. |
| Sync engine | Argo CD | Industry standard. |
| Promotion | Kargo | Stage-to-stage promotion with Git as the record. |
| Policy engine | Kyverno | Admission policy plus image signature verification in one component. |
| Secrets | External Secrets Operator | No secrets in Git, encrypted or otherwise. |
| Secret backend | Chosen in the questionnaire | An in-cluster store for demos, an external store for real use. |
| Interface | Agent-native CLI | The user sets the platform up together with their coding agent. |

## Layers

1. **Bootstrap.** The CLI verifies and applies a minimal Argo CD, then registers
   the user's platform repo. This is the only step that uses the kubeconfig.
2. **Platform.** Argo CD syncs everything else from Git: cert-manager, Kyverno,
   External Secrets Operator, Kargo, and optionally Argo Rollouts.
3. **Tenancy.** A template onboards an application with its own AppProject,
   namespaces, Kargo project and stages, and scoped RBAC.

After bootstrap, Git is the only write path. The bootstrap credentials can be
revoked.

## Agent-native interface

The CLI is built to be driven by an agent on the user's behalf, with the user
approving the decisions that matter.

### Flow

| Step | Command | Mutates cluster |
|---|---|---|
| 1 | `akd preflight` | No |
| 2 | `akd questions` | No |
| 3 | `akd validate` | No |
| 4 | `akd plan` | No |
| 5 | `akd apply --plan <hash>` | Yes |
| 6 | `akd verify` | No |

1. `preflight` inspects the cluster: distro, Kubernetes version, whether the
   kubeconfig has the rights to install, whether NetworkPolicy is enforced,
   whether platform components are already installed, and whether a Gateway
   API or ingress controller is available. Where the API cannot answer a
   question (encryption of Secrets at rest, for example) it says so instead of
   guessing.
2. `questions` emits the questionnaire as JSON, already narrowed by what
   preflight found. Each question carries an id, the prompt, the options, the
   secure default, and a plain-language note on the security consequence of
   each option. The agent walks the user through it.
3. The answers land in one file, `platform.yaml`. `validate` checks it
   against the questionnaire: every question answered, no unknown fields, and
   an acknowledgement for every option that weakens the baseline. `schema`
   prints a JSON Schema generated from the same questionnaire, for editors.
4. `plan` renders every manifest that would be applied and prints a summary
   plus a content hash of the full plan.
5. `apply` only accepts a plan hash. What the user approved is exactly what is
   applied.
6. `verify` audits the live cluster against the baseline.

### Questionnaire topics

- How the Argo CD and Kargo UIs are reached: port-forward only, internal
  gateway, or public with SSO.
- Identity provider for SSO.
- Git host and credential type (GitHub App preferred over deploy keys or
  tokens).
- Secret store backend for External Secrets Operator (see Secrets below).
- Single team or multi-tenant.
- Production promotion policy: pull request with approval, or automatic.
- Optional components.

### Rules that make it safe to hand to an agent

- **No interactive prompts.** Every command takes flags or `platform.yaml` and
  supports `--output json`, with stable exit codes and errors that name the
  fix.
- **Idempotent.** Re-running any command is safe.
- **Secrets never pass through the agent.** `platform.yaml` holds references
  (a secret store path, an environment variable name, a file path), never
  values. The CLI never prints secret values.
- **Weakening a default is explicit.** Choosing a less secure option requires a
  named acknowledgement recorded in `platform.yaml`, so it shows up in review
  and in the `verify` report. An agent cannot downgrade security silently.
- **Cluster content is untrusted.** Strings read from the cluster (names,
  annotations, messages) are length-limited and clearly delimited in output, so
  they cannot pose as instructions to the agent.

### Agent packaging

The repo ships an `AGENTS.md` and a skill that teach an agent the flow above.
Any agent that can run a shell command can drive the tool. An MCP server is a
possible later addition and is not needed for the first version.

## Supply chain

Every third-party component is code running with elevated privileges in the
cluster. The toolkit treats each one as a trust decision.

### Principles

- **Few components.** Each one is listed here with the reason it is included.
  Anything not required is off by default.
- **Nothing floats.** No `latest`, no version ranges, no tags without digests.

### Lock file

`components.lock` records, for every upstream component:

- chart version and chart digest
- every container image by digest
- the expected signer identity and issuer, where upstream signs releases
- whether provenance attestations are available

Releases of the toolkit ship the fully rendered manifests for that lock file,
so an upgrade is a reviewable diff and install does not depend on fetching
charts at run time.

### Verification points

| When | What is checked | By |
|---|---|---|
| Toolkit CI | Signatures, provenance, vulnerability scan, rendered-manifest diff | CI pipeline |
| Before apply | Digests and signatures of everything in the plan | CLI |
| At admission | Image signatures and digests in platform namespaces | Kyverno |
| On demand | Running images match the lock file | `akd verify` |

Kyverno cannot verify itself at admission, so the CLI's pre-apply check is the
control for Kyverno and for the initial Argo CD install.

Where an upstream does not sign its releases, the component is pinned by digest
only, and `verify` reports that gap instead of hiding it.

### Updates

- A dependency bot opens pull requests that bump `components.lock`.
- New upstream releases are held for a cooldown period before they are
  eligible, so a freshly compromised release is more likely to be caught
  upstream first.
- CI re-runs all checks and posts the rendered diff, with new RBAC rules, CRDs
  and webhooks called out.
- Platform upgrades are promoted through Kargo stages like any other change.

### Limits

A valid signature proves who published a release, not that it is benign. A
compromised upstream that signs a malicious release passes signature checks.
The cooldown, the diff review, and the runtime controls below (least-privilege
RBAC, default-deny egress) are what limit the damage in that case.

## Secrets

External Secrets Operator is the only secrets mechanism. Applications and
platform components always consume secrets the same way; only the backend
behind the store definition differs. SOPS is not offered: Argo CD needs a
third-party decryption plugin in the repo-server to use it, and it would put
encrypted secrets in Git.

### Backends

| Backend | Intended use | Secrets exist outside the cluster |
|---|---|---|
| In-cluster store | Demos, evaluation, homelabs | No |
| GCP Secret Manager | Production on GCP | Yes |
| AWS Secrets Manager | Production on AWS | Yes |
| Azure Key Vault | Production on Azure | Yes |
| Vault or OpenBao | Production, self-hosted | Yes |

Preflight suggests the natural backend for the detected distro. The user
confirms the choice in the questionnaire.

### In-cluster store (demo backend)

This backend uses the Kubernetes provider built into External Secrets
Operator, so it adds no extra component.

- Secrets are held as Kubernetes Secrets in a dedicated `secret-store`
  namespace. External Secrets Operator copies them into the namespaces that
  need them.
- The CLI seeds the namespace at bootstrap from local references (an
  environment variable name or a file path). Values never enter Git,
  `platform.yaml`, or the agent's context.
- The namespace is default-deny for network traffic, and only the External
  Secrets Operator service account can read from it.

**The secrets exist only inside the cluster.** This is the important limit of
this backend:

- There is no copy in any external system. If the cluster is lost, the secrets
  are lost and must be re-seeded from their original sources.
- There is no external audit trail, rotation, or versioning.
- Protection at rest is whatever the cluster provides for etcd. The Kubernetes
  API does not reveal whether etcd encryption is on, so preflight reports it
  as unknown and says how to check it on that distro.
- Anyone with cluster-admin can read them.

The CLI states this when the backend is selected, selecting it requires a
recorded acknowledgement in `platform.yaml`, and `akd verify` reports the
platform as running a demo-grade secret backend.

Moving to an external backend later means changing the store definition and
loading the secrets into the new store. No application manifest changes.

## Security baseline

- **Access.** SSO only. The built-in Argo CD admin account is disabled after
  bootstrap. RBAC denies by default. The `default` AppProject is locked.
- **Network.** Default-deny ingress and egress in every platform namespace,
  with explicit allow rules. Nothing is exposed publicly by default.
- **Workloads.** Pod Security `restricted` on platform namespaces. Kyverno
  blocks unsigned images, privileged pods and host access.
- **Secrets.** Delivered by External Secrets Operator from the chosen backend.
  Never stored in Git.
- **Git credentials.** Read-only for Argo CD. Kargo has write access only to
  the repos it promotes, and production promotion goes through a pull request.
- **Tenancy.** Each tenant's AppProject restricts source repos, destination
  namespaces and resource kinds.

`akd verify` checks each item and maps it to the Argo CD security guidance and
the CIS and NSA Kubernetes benchmarks.

## Documents to write

- Threat model: what each control defends against, and what is out of scope.
- Component register: each third-party component, why it is included, and what
  it can access.

## Open questions

1. Name of the CLI.
2. Length of the update cooldown.
