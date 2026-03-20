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

## How reconcile works

1. Read namespace label `namespaceclass.akuity.io/name`.
2. Load corresponding `NamespaceClass`.
3. Parse each template as unstructured Kubernetes object.
4. Resolve GVR via RESTMapper and create/update object.
5. Track managed object references in a namespace-local ConfigMap (`namespaceclass-inventory`).
6. Delete objects that existed in prior inventory but are absent in current desired state.

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
4. Approval controller validates risk policy and applies `proposedResources` into `NamespaceClass.spec.resources`.
5. Namespace controller reconciles namespaces from updated class spec.

## Drift detection details

The watcher performs real object-level comparison for each namespace using a class:

1. Resolve each template resource to GVR via RESTMapper.
2. Read the actual object from target namespace (or cluster scope).
3. Compare normalized expected vs actual values using a kind-based strategy.
4. Report drift for:
   - missing resource
   - field mismatch (`spec`/`data`/`rules`/etc.)
   - inventory mismatch (missing managed / extra managed)

### Current field strategies

- `ConfigMap` / `Secret`: compare `data` (and `binaryData` when present)
- `Role` / `ClusterRole`: compare normalized `rules`
- `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration`: compare normalized `webhooks`
- `ServiceAccount`: compare `automountServiceAccountToken`, `imagePullSecrets`, `secrets`
- `ResourceQuota`: compare whitelisted `spec.hard`, `spec.scopes`, `spec.scopeSelector`
- `LimitRange`: compare whitelisted `spec.limits[*]` keys
- default fallback: compare `spec`

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

## End-to-end demo flow

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

```bash
kubectl -n web-portal delete networkpolicy allow-public-ingress
```

### 4) Wait for watcher analysis

No manual trigger is needed in current implementation. Watcher continuously checks drift.

```bash
kubectl get namespaceclass public-network -o yaml
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

## Reset Kubernetes resources for this project

Use this to clean up all demo resources created by this repository and return the cluster to a fresh state.

Delete sample resources first:

```bash
kubectl delete -f config/samples/namespaceclass-change-request.yaml --ignore-not-found
kubectl delete -f config/samples/namespaceclass-change-request-internal.yaml --ignore-not-found
kubectl delete -f config/samples/namespace-web-portal.yaml --ignore-not-found
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
```

Verify everything is removed:

```bash
kubectl get namespaceclass
kubectl get namespaceclasschangerequest
kubectl get ns web-portal
```
