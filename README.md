# Namespace Class Controller (Go)

This project implements a Kubernetes `NamespaceClass` CRD and controller.

## What it does

- Adds a cluster-scoped CRD: `NamespaceClass` (`akuity.io/v1alpha1`)
- Lets operators define arbitrary resource templates in `spec.resources`
- Watches `Namespace` objects with label `namespaceclass.akuity.io/name`
- Creates/updates resources in the target namespace from the selected class
- Handles class switching by deleting previously managed resources that are no longer desired
- Handles class updates by reconciling all namespaces using that class
- Runs an AI watcher that detects drift and writes proposals to `NamespaceClass.status`
- Adds approval CRD `NamespaceClassChangeRequest` for human/GitOps approval before applying proposals
- Blocks high-risk resources by default (`Role`, `ClusterRole`, `*Webhook*`, `CRD`)
- Uses object-level drift detection (actual object comparison, not only inventory keys)
- Supports per-kind field strategies via registry (`map + matcher`) for easier future extension

## API shape

The CRD allows flexible templates:

```yaml
apiVersion: akuity.io/v1alpha1
kind: NamespaceClass
metadata:
  name: public-network
spec:
  resources:
    - apiVersion: networking.k8s.io/v1
      kind: NetworkPolicy
      metadata:
        name: allow-public-ingress
      spec:
        podSelector: {}
        policyTypes: ["Ingress"]
        ingress:
          - from:
              - ipBlock:
                  cidr: 0.0.0.0/0
```

The same `spec.resources` list can mix kinds (for example `ServiceAccount`, `ConfigMap`, `LimitRange`, `ResourceQuota`). See `config/samples/namespaceclass-multikind-v1.yaml` and `namespaceclass-multikind-v2.yaml`.

**PodSecurityPolicy:** Kubernetes 1.25+ removed `PodSecurityPolicy`; prefer Pod Security admission, `LimitRange`, `ResourceQuota`, and `NetworkPolicy` in class templates instead.

## How reconcile works

1. Read namespace label `namespaceclass.akuity.io/name`.
2. Load corresponding `NamespaceClass`.
3. Parse each template as unstructured Kubernetes object.
4. Resolve GVR via RESTMapper and create/update object.
5. Track managed object references in a namespace-local ConfigMap (`namespaceclass-inventory`).
6. Delete objects that existed in prior inventory but are absent in current desired state.
7. Reconcile when a **managed child object** (same label; not the inventory `ConfigMap`) is created, updated, or deleted, so accidental deletes of templates such as `ServiceAccount` are repaired without relabeling the namespace.

This inventory model supports:

- Namespace switching from class A -> class B.
- NamespaceClass edits that add/remove templates.

High-risk resources are blocked by default. To allow them, explicit approval flow must set `allowHighRisk=true` in `NamespaceClassChangeRequest`.

## AI + approval flow

1. AI watcher detects drift for each class.
2. AI watcher writes:
   - `status.conditions` (`DriftDetected`, `RecommendationPending`)
   - `status.recommendations[]` with `id`, `summary`, `diff`, `proposedResources`
3. Operator/GitOps creates `NamespaceClassChangeRequest` with:
   - `spec.namespaceClassName`
   - `spec.recommendationID`
   - `spec.approved=true`
   - optional `spec.pullRequestURL` (merged PR link); **required** when PR gating is on (see below)
4. Approval controller validates risk policy (and pull request URL when required), then applies `proposedResources` into `NamespaceClass.spec.resources`.
5. Namespace controller reconciles namespaces from updated class spec.

### Pull request gating

To require a **merged pull request** (or any `http(s)` review link) before AI proposals can be applied:

- **Per class:** set on the `NamespaceClass`:

  `metadata.annotations.namespaceclass.akuity.io/require-pull-request: "true"`

- **Cluster-wide:** run the manager with env `NAMESPACECLASS_REQUIRE_PULL_REQUEST_URL` set to `1`, `true`, or `yes`.

Then each approved `NamespaceClassChangeRequest` must include `spec.pullRequestURL` with a valid `http` or `https` URL. On success, `status.appliedPullRequestURL` records what was used for audit.

## Drift detection details

The AI watcher walks **every** entry in `NamespaceClass.spec.resources` for each namespace labeled with that class (not only `NetworkPolicy`): `ServiceAccount`, `ConfigMap`, `LimitRange`, `ResourceQuota`, and any other template kind resolve through the RESTMapper and use a dedicated compare strategy when one exists, otherwise raw `spec`.

The watcher performs real object-level comparison for each namespace using a class:

1. Resolve each template resource to GVR via RESTMapper.
2. Read the actual object from target namespace (or cluster scope).
3. Compare normalized expected vs actual values using a kind-based strategy.
4. Report drift for:
   - missing resource
   - field mismatch (`spec`/`data`/`rules`/etc.)
   - inventory mismatch (missing managed / extra managed)

### Current field strategies (not only NetworkPolicy)

The registry in `internal/aiwatcher/compare_strategy.go` applies **per Kind**. Anything in `spec.resources` is analyzed; below are the kinds with **custom** normalization (everything else uses the **default**: whole `spec`).

| Kind                                                                       | What the AI compares (normalized)                                       |
| -------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| `ServiceAccount`                                                           | `automountServiceAccountToken`, `imagePullSecrets`, `secrets`           |
| `ConfigMap`                                                                | `data`, `binaryData`                                                    |
| `Secret`                                                                   | `data`, `binaryData`                                                    |
| `LimitRange`                                                               | `spec.limits` (selected keys, stable order)                             |
| `ResourceQuota`                                                            | `spec.hard`, `spec.scopes`, `spec.scopeSelector`                        |
| `NetworkPolicy`                                                            | `podSelector`, `policyTypes`, `ingress`, `egress` (rules stable-sorted) |
| `Role` / `ClusterRole`                                                     | `rules`                                                                 |
| `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration`          | selected `webhooks` fields                                              |
| **Any other Kind** (e.g. `PodDisruptionBudget`, `Service`, CRDs you allow) | entire `spec` as JSON-shaped structure                                  |

#### PodSecurityPolicy

`PodSecurityPolicy` was **removed in Kubernetes 1.25** and is not a practical template kind on current clusters. This project does not ship a PSP-specific strategy: if you still run an ancient API server with PSP, drift would fall back to **raw `spec` comparison**—but the assignment’s intent is better met with **Pod Security admission**, `LimitRange`, `ResourceQuota`, and `NetworkPolicy`, which are all covered above (or via default `spec`).

### Extending strategies

Strategy registry lives in `internal/aiwatcher/compare_strategy.go`.

To support a new Kind, add one strategy entry in `compareStrategies` with:

- `matcher` (e.g. `matchKinds("poddisruptionbudget")`)
- `extractor` (normalization + field selection function)

No change is needed in the watcher reconcile loop.

## Files

- `main.go` - manager bootstrap + optional AI watcher
- `internal/controller/namespaceclass_controller.go` - core reconcile logic
- `internal/controller/changerequest_controller.go` - approval CRD reconciler
- `internal/aiwatcher/watcher.go` - polling drift watcher with AI proposal + status updates
- `internal/aiwatcher/compare_strategy.go` - per-kind comparison strategy registry
- `config/crd/bases/akuity.io_namespaceclasses.yaml` - CRD manifest
- `config/crd/bases/akuity.io_namespaceclasschangerequests.yaml` - approval CRD manifest
- `config/samples/*` - sample classes + namespace

## Run locally

```bash
go mod tidy
go run .
```

## Apply CRD and samples

```bash
kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
kubectl apply -f config/samples/namespaceclass-public-internal.yaml
kubectl apply -f config/samples/namespace-web-portal.yaml
```

Then approve a recommendation:

```bash
kubectl get namespaceclass public-network -o jsonpath='{.status.recommendations[0].id}'
kubectl apply -f config/samples/namespaceclass-change-request.yaml
```

## AI watcher

Set:

- `OPENAI_API_KEY`

Optional flags:

- `--ai-poll-interval=30s`

When enabled, watcher polls classes and namespaces, detects drift, asks OpenAI for a structured proposal, and writes it into `NamespaceClass.status`.

## Notes

- The watcher currently compares against desired templates and managed inventory; it does not perform SSA-style field ownership tracking.
- `Secret` comparison uses persisted `data` (not `stringData`, which is write-only).

## End-to-end demo flows

### Test scripts

| Script (Bash)                  | Script (Windows `cmd`)        | What it runs                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------ | ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `./test-flow-networkpolicy.sh` | `test-flow-networkpolicy.cmd` | `web-portal` + `public-network` / `internal-network` NetworkPolicies, class switching, AI recommendations, and `NamespaceClassChangeRequest` approval (requires controller with `OPENAI_API_KEY`).                                                                                                                                                                                                              |
| `./test-flow-multikind.sh`     | `test-flow-multikind.cmd`     | `app-sandbox` + `multi-kind-demo` class: `ServiceAccount` + `ConfigMap`, then a **NamespaceClass spec upgrade** (adds `LimitRange`, updates `ConfigMap` data), then deletes the managed `ServiceAccount` to show reconcile recreate. No AI approval steps.                                                                                                                                                      |
| `./test-flow-multikind-ai.sh`  | `test-flow-multikind-ai.cmd`  | Same class/namespace as above, but drives the **AI drift → recommendation → `NamespaceClassChangeRequest`** loop with **pull request gating**. Enables short `ai-drift-hold-seconds`, then mutates `ConfigMap` + deletes managed `ServiceAccount` to produce stable drift and recovery proposal. Pauses for you to paste `recommendationID` into `config/samples/namespaceclass-change-request-multikind.yaml`. |
| `./test-flow.sh`               | `test-flow.cmd`               | Convenience wrapper; same as the NetworkPolicy script above.                                                                                                                                                                                                                                                                                                                                                    |

The numbered steps **1–7** below mirror `./test-flow-networkpolicy.sh` / `test-flow-networkpolicy.cmd` (that script pauses for you to paste recommendation IDs into the sample YAML files).

### 1) Start controller with AI watcher

```bash
export OPENAI_API_KEY=your-key
go run . --ai-poll-interval=2s --metrics-bind-address=:18080
```

If ports are already in use on your machine, override both ports:

```bash
go run . --ai-poll-interval=2s --metrics-bind-address=:28080 --health-probe-bind-address=:28081
```

### 2) Apply base resources

```bash
kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
kubectl apply -f config/samples/namespaceclass-public-internal.yaml
kubectl apply -f config/samples/namespace-web-portal.yaml
```

### 3) Create drift (for AI to detect)

For example, delete a managed policy in `web-portal`:

Optional (recommended for demo stability): pause namespace auto-heal briefly so watcher can
consistently capture drift before resources are reconciled back.

```bash
kubectl annotate namespaceclass public-network namespaceclass.akuity.io/ai-drift-hold-seconds=45 --overwrite
```

`ai-drift-hold-seconds` is one-shot per class switch: after the hold window expires, auto-heal resumes
and will not keep extending the hold indefinitely.

```bash
kubectl -n web-portal delete networkpolicy allow-public-ingress
```

### 4) Wait for watcher analysis

No manual trigger is needed in current implementation. Watcher continuously checks drift.

```bash
kubectl get namespaceclass public-network -o yaml
kubectl -n web-portal get networkpolicy allow-public-ingress
```

You should see:

- `status.conditions` with `DriftDetected` / `RecommendationPending`
- `status.recommendations[]` with `id`, `summary`, `diff`, `proposedResources`

### 5) Approve recommendation

Get recommendation id:

```bash
kubectl get namespaceclass public-network -o jsonpath='{.status.recommendations[0].id}{"\n"}'
```

Windows `cmd.exe` note (different quote/escape rules):

```bash
kubectl get namespaceclass public-network -o jsonpath="{.status.recommendations[0].id}"
```

Put that value into `config/samples/namespaceclass-change-request.yaml` under `spec.recommendationID`, then apply:

```bash
kubectl apply -f config/samples/namespaceclass-change-request.yaml
```

### 6) Verify apply result

```bash
kubectl get namespaceclasschangerequest public-network-approval -o yaml
kubectl get namespaceclass public-network -o yaml
kubectl -n web-portal get networkpolicy
```

If the change request phase is `Applied` and resources are restored/updated, the full loop is working.

### 7) Switch between networks (public <-> internal)

This demonstrates namespace class switching without AI approval flow. The namespace controller will
remove resources from the old class and apply resources from the new class.

Switch `web-portal` to `internal-network`:

```bash
kubectl label namespace web-portal namespaceclass.akuity.io/name=internal-network --overwrite
kubectl -n web-portal get networkpolicy
kubectl -n web-portal get networkpolicy allow-public-ingress
kubectl -n web-portal get networkpolicy allow-vpn-only -o yaml
```

Expected result after reconcile:

- `allow-public-ingress` is deleted (old class resource).
- `allow-vpn-only` exists (new class resource).
- For up to 10 minutes after switch, AI watcher reports a switch drift on the new class (so you can review/approve it).

Switch recommendation strategy:

```bash
# sample defaults to rollback-oriented recommendation (suggest previous class resources)
# because config/samples/namespaceclass-public-internal.yaml sets this annotation:
kubectl annotate namespaceclass internal-network namespaceclass.akuity.io/switch-drift-mode=suggest-rollback-to-previous --overwrite

# optional override if you want recommendation to keep current class template instead:
kubectl annotate namespaceclass internal-network namespaceclass.akuity.io/switch-drift-mode=confirm-current-class --overwrite
```

Wait for watcher analysis on `internal-network`:

```bash
kubectl get namespaceclass internal-network -o yaml
```

You should see:

- `status.conditions` with `DriftDetected` / `RecommendationPending`
- `status.recommendations[]` with `id`, `summary`, `diff`, `proposedResources`

Approve switch recommendation:

```bash
kubectl get namespaceclass internal-network -o jsonpath='{.status.recommendations[0].id}{"\n"}'
```

Windows `cmd.exe` note (different quote/escape rules):

```bash
kubectl get namespaceclass internal-network -o jsonpath="{.status.recommendations[0].id}"
```

Put the recommendation id from the step above into
`config/samples/namespaceclass-change-request-internal.yaml` under `spec.recommendationID`, then apply:

```bash
kubectl apply -f config/samples/namespaceclass-change-request-internal.yaml
```

After approval is `Applied`, controller also rolls matching namespaces back to their previous class label.
For example, `web-portal` can switch from `internal-network` back to `public-network` automatically.

Switch back to `public-network`:

```bash
kubectl label namespace web-portal namespaceclass.akuity.io/name=public-network --overwrite
kubectl -n web-portal get networkpolicy
kubectl -n web-portal get networkpolicy allow-vpn-only
kubectl -n web-portal get networkpolicy allow-public-ingress -o yaml
```

Expected result after reconcile:

- `allow-vpn-only` is deleted.
- `allow-public-ingress` is recreated.

### 8) Multi-kind templates: ServiceAccount, ConfigMap, LimitRange (class spec update)

This flow shows **updating `NamespaceClass.spec.resources`** for kinds other than `NetworkPolicy`, and confirms namespaces on that class pick up creates/updates. It uses the `multi-kind-demo` class and `app-sandbox` namespace.

**PodSecurityPolicy** is not used here (removed in modern clusters); `LimitRange` illustrates a namespace-scoped policy-style object alongside identity (`ServiceAccount`) and config (`ConfigMap`).

1. With the controller running (AI watcher optional for this flow), apply CRDs if needed, then the class and namespace:

```bash
kubectl apply -f config/crd/bases/akuity.io_namespaceclasses.yaml
kubectl apply -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml
kubectl apply -f config/samples/namespaceclass-multikind-v1.yaml
kubectl apply -f config/samples/namespace-app-sandbox.yaml
```

2. Verify v1 objects (`ConfigMap` key `data.profile` should be `v1`; no `LimitRange` yet).

On **Windows `cmd.exe`**, do not copy the Bash `jsonpath='...'` line: `cmd` passes the quotes differently, and `{\"\\n\"}` / `{\"\n\"}` inside jsonpath breaks parsing (`unrecognized character ... '\\'`). Use **double quotes** around the whole jsonpath expression:

```bat
kubectl -n app-sandbox get serviceaccount app-runner
kubectl -n app-sandbox get configmap class-config -o jsonpath="{.data.profile}"
kubectl -n app-sandbox get limitrange
```

Git Bash / WSL / macOS / Linux (single quotes; optional newline in output):

```bash
kubectl -n app-sandbox get serviceaccount app-runner
kubectl -n app-sandbox get configmap class-config -o jsonpath='{.data.profile}{"\n"}'
kubectl -n app-sandbox get limitrange
```

3. Apply the **v2** class definition (adds `LimitRange`, updates `ConfigMap` data):

```bash
kubectl apply -f config/samples/namespaceclass-multikind-v2.yaml
```

This step is a normal class upgrade and should reconcile directly; by itself it does **not** create AI drift recommendation.
If you want AI to recommend changing configuration back to class template, continue with [Multi-kind + AI watcher + pull request gate](#multi-kind--ai-watcher--pull-request-gate).

4. After reconcile, expect `profile=v2`, new `data.note`, and `LimitRange` `mem-limit-range`:

```bash
kubectl -n app-sandbox get configmap class-config -o yaml
kubectl -n app-sandbox get limitrange mem-limit-range -o yaml
```

5. Optional **drift** check: delete the managed `ServiceAccount`; the controller should enqueue a reconcile for that namespace and recreate `app-runner` (it watches common managed kinds: `ServiceAccount`, `ConfigMap`, `LimitRange`, `ResourceQuota`, `NetworkPolicy`, labeled `namespaceclass.akuity.io/managed=true`, and excludes the inventory `ConfigMap`).

```bash
kubectl -n app-sandbox delete serviceaccount app-runner
# retry briefly; recreate is event-driven after the delete is observed
kubectl -n app-sandbox get serviceaccount app-runner
```

If nothing comes back, confirm the **rebuilt controller** is running against the same cluster (older binaries only watched `Namespace` / `NamespaceClass`, so child deletes did not trigger reconcile).

#### Multi-kind + AI watcher + pull request gate

This extends the same `multi-kind-demo` / `app-sandbox` setup to exercise **drift detection**, **OpenAI proposal** (written to `NamespaceClass.status.recommendations`), **`NamespaceClassChangeRequest` approval**, and **`spec.pullRequestURL`** when PR gating is on. Run the manager with `OPENAI_API_KEY` set (see [§1 Start controller with AI watcher](#1-start-controller-with-ai-watcher)).

1. Apply CRDs, `namespaceclass-multikind-v1.yaml`, and `namespace-app-sandbox.yaml` (same as steps above). Wait until `profile=v1` and `ServiceAccount` `app-runner` exist.

2. **Enable PR gating** for this class (pick one):
   - Per class:

     ```bash
     kubectl annotate namespaceclass multi-kind-demo namespaceclass.akuity.io/require-pull-request=true --overwrite
     ```

   - Or cluster-wide: start the manager with `NAMESPACECLASS_REQUIRE_PULL_REQUEST_URL` set to `1`, `true`, or `yes`.

3. **Create drift** the same way as NetworkPolicy flow: temporarily hold auto-heal, then mutate/delete managed objects so watcher can recommend restoring the class template.

   ```bash
   kubectl annotate namespaceclass multi-kind-demo namespaceclass.akuity.io/ai-drift-hold-seconds=45 --overwrite
   kubectl -n app-sandbox patch configmap class-config --type merge -p '{"data":{"profile":"tampered-v0","note":"manual-drift"}}'
   kubectl -n app-sandbox delete serviceaccount app-runner
   ```

4. Poll until the class has a pending recommendation (same `jsonpath` caveats as [§5 Approve recommendation](#5-approve-recommendation) on Windows):

   ```bash
   kubectl get namespaceclass multi-kind-demo -o yaml
   kubectl get namespaceclass multi-kind-demo -o jsonpath='{.status.recommendations[0].id}{"\n"}'
   ```

   Windows `cmd.exe`:

   ```bat
   kubectl get namespaceclass multi-kind-demo -o jsonpath="{.status.recommendations[0].id}"
   ```

5. Copy that `id` into `config/samples/namespaceclass-change-request-multikind.yaml` as `spec.recommendationID`. Keep `spec.pullRequestURL` as a valid `http`/`https` URL when gating is on (the sample uses a placeholder PR URL; substitute your real merged PR link in real workflows). Apply:

   ```bash
   kubectl apply -f config/samples/namespaceclass-change-request-multikind.yaml
   ```

6. Confirm `status.phase` is `Applied`, `status.appliedPullRequestURL` is recorded, `NamespaceClass.spec.resources` reflects the approved proposal, and managed objects are restored from class template (`profile=v1`, `app-runner` exists):

   ```bash
   kubectl get namespaceclasschangerequest multikind-demo-approval -o yaml
   kubectl get namespaceclass multi-kind-demo -o yaml
   kubectl -n app-sandbox get serviceaccount app-runner
   kubectl -n app-sandbox get configmap class-config -o jsonpath='{.data.profile}{"\n"}'
   ```

**Note:** The model’s `proposedResources` may differ slightly from the checked-in v1 YAML; the approval controller still replaces `spec.resources` with the recommendation. If the change request is `Rejected`, check `status.message` (e.g. high-risk kinds, bad `pullRequestURL`, or `recommendationID` mismatch).

Automated version: `./test-flow-multikind.sh` or `test-flow-multikind.cmd` (spec upgrade + drift recreate only). For AI + PR gate: `./test-flow-multikind-ai.sh` or `test-flow-multikind-ai.cmd` (requires `OPENAI_API_KEY` in the environment **for the script’s preflight check** and in the environment where you run the manager).

### 9) ResourceQuota (optional patch)

To try another namespaced kind without editing the repo, patch `multi-kind-demo` after the multikind flow (or add a template and apply). Example fragment:

```yaml
- apiVersion: v1
  kind: ResourceQuota
  metadata:
    name: compute-quota
  spec:
    hard:
      pods: "10"
```

Merge it into `spec.resources` with `kubectl edit namespaceclass multi-kind-demo` (or a GitOps commit), then confirm `kubectl -n app-sandbox get resourcequota`.

## Reset Kubernetes resources for this project

Use this to clean up all demo resources created by this repository and return the cluster to a fresh state.

Delete sample resources first:

```bash
kubectl delete -f config/samples/namespaceclass-change-request.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-change-request-internal.yaml --ignore-not-found
kubectl delete -f config/samples/namespace-web-portal.yaml --ignore-not-found
kubectl delete -f config/samples/namespace-app-sandbox.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-multikind-v2.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-multikind-v1.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-public-internal.yaml --ignore-not-found
```

Delete CRDs (this also removes all corresponding custom resources):

```bash
kubectl delete -f config/crd/bases/akuity.io_namespaceclasschangerequests.yaml --ignore-not-found
kubectl delete -f config/crd/bases/akuity.io_namespaceclasses.yaml --ignore-not-found
```

Optional force cleanup for leftovers in demo namespace:

```bash
kubectl -n web-portal delete configmap namespaceclass-inventory --ignore-not-found
kubectl -n web-portal delete networkpolicy allow-public-ingress allow-vpn-only --ignore-not-found
kubectl delete namespace web-portal --ignore-not-found
kubectl delete namespace app-sandbox --ignore-not-found
```

Verify everything is removed:

```bash
kubectl get namespaceclass
kubectl get namespaceclasschangerequest
kubectl get ns web-portal
kubectl get ns app-sandbox
```
