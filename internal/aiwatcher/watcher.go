package aiwatcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
)

type Watcher struct {
	dynClient  dynamic.Interface
	mapper     *restmapper.DeferredDiscoveryRESTMapper
	interval   time.Duration
	lastDigest map[string]string
	httpClient *http.Client
}

const switchDriftWindow = 10 * time.Minute

const (
	switchDriftModeAnnotation = "namespaceclass.akuity.io/switch-drift-mode"
	switchModeConfirmCurrent  = "confirm-current-class"
	switchModeSuggestRollback = "suggest-rollback-to-previous"
)

type switchDriftHint struct {
	namespace string
	prevClass string
}

func New(cfg *rest.Config, interval time.Duration) *Watcher {
	return &Watcher{
		dynClient:  dynamic.NewForConfigOrDie(cfg),
		mapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(cfg))),
		interval:   interval,
		lastDigest: map[string]string{},
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (w *Watcher) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("ai-watcher")
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	log.Info("AI watcher started", "interval", w.interval.String())

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			classes, err := w.listNamespaceClasses(ctx)
			if err != nil {
				log.Error(err, "failed to list namespaceclasses")
				continue
			}
			for i := range classes.Items {
				if err := w.reconcileClass(ctx, &classes.Items[i]); err != nil {
					log.Error(err, "class analyze failed", "class", classes.Items[i].GetName())
				}
			}
		}
	}
}

func (w *Watcher) NeedLeaderElection() bool { return false }

func (w *Watcher) listNamespaceClasses(ctx context.Context) (*unstructured.UnstructuredList, error) {
	return w.dynClient.Resource(schema.GroupVersionResource{Group: "akuity.io", Version: "v1alpha1", Resource: "namespaceclasses"}).List(ctx, metav1.ListOptions{})
}

func (w *Watcher) reconcileClass(ctx context.Context, classObj *unstructured.Unstructured) error {
	className := classObj.GetName()
	currentResources := readSpecResources(classObj)
	switchMode := readSwitchDriftMode(classObj)

	drift, hints, err := w.detectDrift(ctx, className, currentResources)
	if err != nil {
		return err
	}

	analysisDigest, err := buildAnalysisDigest(currentResources, drift)
	if err != nil {
		return err
	}
	if w.lastDigest[className] == analysisDigest {
		return nil
	}
	w.lastDigest[className] = analysisDigest

	if len(drift) == 0 {
		return w.writeClassStatus(ctx, classObj, "", nil, currentResources, false)
	}

	proposedResources := currentResources
	if switchMode == switchModeSuggestRollback {
		if rollbackResources, ok := w.rollbackProposalResources(ctx, hints); ok {
			proposedResources = rollbackResources
		}
	}

	proposal, err := w.askOpenAIForProposal(ctx, className, drift, proposedResources)
	if err != nil {
		return err
	}
	if len(proposal.ProposedResources) == 0 {
		proposal.ProposedResources = proposedResources
	}
	return w.writeClassStatus(ctx, classObj, proposal.Summary, proposal.Diff, proposal.ProposedResources, true)
}

func buildAnalysisDigest(currentResources []any, drift []string) (string, error) {
	payload := map[string]any{
		"resources": currentResources,
		"drift":     drift,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return sha(b), nil
}

func (w *Watcher) detectDrift(ctx context.Context, className string, current []any) ([]string, []switchDriftHint, error) {
	nsList, err := w.dynClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).List(ctx, metav1.ListOptions{
		LabelSelector: "namespaceclass.akuity.io/name=" + className,
	})
	if err != nil {
		return nil, nil, err
	}
	templates, err := buildTemplateTargetsForWatcher(w, current)
	if err != nil {
		return nil, nil, err
	}
	desired := toResourceRefKeys(current)
	drift := []string{}
	hints := []switchDriftHint{}
	for _, ns := range nsList.Items {
		nsName := ns.GetName()
		if switchDrift, hint := buildSwitchDriftMessage(&ns, className); switchDrift != "" {
			drift = append(drift, switchDrift)
			if hint != nil {
				hints = append(hints, *hint)
			}
		}
		for _, tpl := range templates {
			resourceClient := w.dynClient.Resource(tpl.resource)
			var readClient dynamic.ResourceInterface = resourceClient
			if tpl.namespaced {
				readClient = resourceClient.Namespace(nsName)
			}
			actual, err := readClient.Get(ctx, tpl.name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					drift = append(drift, fmt.Sprintf("namespace %s missing resource %s/%s", nsName, tpl.kind, tpl.name))
					continue
				}
				drift = append(drift, fmt.Sprintf("namespace %s cannot read resource %s/%s: %v", nsName, tpl.kind, tpl.name, err))
				continue
			}
			actualValue := tpl.extractor(actual.Object)
			if !reflect.DeepEqual(normalizeJSONValue(tpl.expected), normalizeJSONValue(actualValue)) {
				drift = append(drift, fmt.Sprintf("namespace %s %s drift on %s/%s", nsName, tpl.compareOn, tpl.kind, tpl.name))
			}
		}

		cm, err := w.dynClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace(nsName).Get(ctx, "namespaceclass-inventory", metav1.GetOptions{})
		if err != nil {
			drift = append(drift, fmt.Sprintf("namespace %s missing inventory configmap", nsName))
			continue
		}
		data, found, _ := unstructured.NestedStringMap(cm.Object, "data")
		if !found {
			drift = append(drift, fmt.Sprintf("namespace %s inventory data missing", nsName))
			continue
		}
		raw := data["resources"]
		if strings.TrimSpace(raw) == "" {
			drift = append(drift, fmt.Sprintf("namespace %s inventory resources empty", nsName))
			continue
		}
		var refs []map[string]any
		if err := json.Unmarshal([]byte(raw), &refs); err != nil {
			drift = append(drift, fmt.Sprintf("namespace %s inventory parse error", nsName))
			continue
		}
		actual := map[string]struct{}{}
		for _, ref := range refs {
			key := fmt.Sprintf("%v|%v", ref["kind"], ref["name"])
			actual[key] = struct{}{}
		}
		for k := range desired {
			if _, ok := actual[k]; !ok {
				drift = append(drift, fmt.Sprintf("namespace %s missing managed resource %s", nsName, k))
			}
		}
		for k := range actual {
			if _, ok := desired[k]; !ok {
				drift = append(drift, fmt.Sprintf("namespace %s has extra managed resource %s", nsName, k))
			}
		}
	}
	sort.Strings(drift)
	return drift, hints, nil
}

func buildSwitchDriftMessage(ns *unstructured.Unstructured, className string) (string, *switchDriftHint) {
	ann := ns.GetAnnotations()
	if ann == nil {
		return "", nil
	}
	prevClass := strings.TrimSpace(ann["namespaceclass.akuity.io/previous-class"])
	switchedAt := strings.TrimSpace(ann["namespaceclass.akuity.io/switched-at"])
	if prevClass == "" || prevClass == className || switchedAt == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, switchedAt)
	if err != nil {
		return "", nil
	}
	if time.Since(t) > switchDriftWindow {
		return "", nil
	}
	msg := fmt.Sprintf(
		"namespace %s switched from class %s to %s (within %s window)",
		ns.GetName(),
		prevClass,
		className,
		switchDriftWindow.String(),
	)
	return msg, &switchDriftHint{namespace: ns.GetName(), prevClass: prevClass}
}

func readSwitchDriftMode(classObj *unstructured.Unstructured) string {
	ann := classObj.GetAnnotations()
	if ann == nil {
		return switchModeConfirmCurrent
	}
	mode := strings.TrimSpace(strings.ToLower(ann[switchDriftModeAnnotation]))
	if mode == switchModeSuggestRollback {
		return mode
	}
	return switchModeConfirmCurrent
}

func (w *Watcher) rollbackProposalResources(ctx context.Context, hints []switchDriftHint) ([]any, bool) {
	if len(hints) == 0 {
		return nil, false
	}
	// If multiple namespaces switched from different classes, keep default mode to avoid mixed proposals.
	prevClass := hints[0].prevClass
	for _, h := range hints[1:] {
		if h.prevClass != prevClass {
			return nil, false
		}
	}
	u, err := w.dynClient.Resource(schema.GroupVersionResource{Group: "akuity.io", Version: "v1alpha1", Resource: "namespaceclasses"}).
		Get(ctx, prevClass, metav1.GetOptions{})
	if err != nil {
		return nil, false
	}
	res, found, _ := unstructured.NestedSlice(u.Object, "spec", "resources")
	if !found || len(res) == 0 {
		return nil, false
	}
	return res, true
}

func readSpecResources(item *unstructured.Unstructured) []any {
	res, found, _ := unstructured.NestedSlice(item.Object, "spec", "resources")
	if !found {
		return nil
	}
	return res
}

func (w *Watcher) askOpenAI(ctx context.Context, prompt string) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is empty")
	}

	body := map[string]any{
		"model": "gpt-4.1-mini",
		"input": []map[string]any{
			{
				"role": "system",
				"content": []map[string]string{
					{"type": "input_text", "text": "You are a Kubernetes SRE assistant. Be concise."},
				},
			},
			{
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": prompt},
				},
			},
		},
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai status=%d body=%s", resp.StatusCode, string(rb))
	}

	var decoded map[string]any
	if err := json.Unmarshal(rb, &decoded); err != nil {
		return "", err
	}

	if outputText, ok := decoded["output_text"].(string); ok && outputText != "" {
		return outputText, nil
	}
	return string(rb), nil
}

type aiProposal struct {
	Summary           string `json:"summary"`
	Diff              []any  `json:"diff"`
	ProposedResources []any  `json:"proposedResources"`
}

func (w *Watcher) askOpenAIForProposal(ctx context.Context, className string, drift []string, current []any) (*aiProposal, error) {
	payload := map[string]any{
		"className":         className,
		"drift":             drift,
		"currentResources":  current,
		"requiredResponse":  `{"summary":"...","diff":[...],"proposedResources":[...]}`,
		"safetyConstraints": "Do not include Role/ClusterRole/CRD/*Webhook* unless explicitly needed.",
		"changeProcess":     "Cluster apply of proposals goes through NamespaceClassChangeRequest; teams should commit template updates via git and merge a pull request, then set spec.pullRequestURL on the change request when PR gating is enabled (annotation namespaceclass.akuity.io/require-pull-request or env NAMESPACECLASS_REQUIRE_PULL_REQUEST_URL).",
	}
	b, _ := json.Marshal(payload)
	text, err := w.askOpenAI(ctx, "Given this namespace class drift, return ONLY valid JSON:\n"+string(b))
	if err != nil {
		return nil, err
	}
	var out aiProposal
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return &aiProposal{
			Summary:           "AI output was non-JSON; using current resources as fallback proposal.",
			Diff:              []any{"fallback-noop"},
			ProposedResources: current,
		}, nil
	}
	return &out, nil
}

func (w *Watcher) writeClassStatus(ctx context.Context, classObj *unstructured.Unstructured, summary string, diff []any, proposed []any, pending bool) error {
	classRes := schema.GroupVersionResource{Group: "akuity.io", Version: "v1alpha1", Resource: "namespaceclasses"}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := w.dynClient.Resource(classRes).Get(ctx, classObj.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		now := metav1.Now().Format(time.RFC3339)
		recs := []any{}
		if pending {
			recs = []any{map[string]any{
				"id":                 fmt.Sprintf("rec-%s", sha([]byte(classObj.GetName() + now))[:12]),
				"status":             "Pending",
				"createdAt":          now,
				"summary":            summary,
				"diff":               diff,
				"proposedResources":  proposed,
				"recommendationHash": sha(mustJSON(proposed)),
			}}
		} else {
			existing, found, _ := unstructured.NestedSlice(latest.Object, "status", "recommendations")
			if found && hasPendingRecommendation(existing) {
				recs = existing
			}
		}

		condStatus := "False"
		condMsg := "No drift detected"
		if pending || hasPendingRecommendation(recs) {
			condStatus = "True"
			condMsg = "Drift detected; pending approval"
		}

		_ = unstructured.SetNestedSlice(latest.Object, []any{
			map[string]any{
				"type":               "DriftDetected",
				"status":             condStatus,
				"reason":             "AIDriftAnalysis",
				"message":            condMsg,
				"lastTransitionTime": now,
			},
			map[string]any{
				"type":               "RecommendationPending",
				"status":             condStatus,
				"reason":             "AIProposal",
				"message":            condMsg,
				"lastTransitionTime": now,
			},
		}, "status", "conditions")
		_ = unstructured.SetNestedSlice(latest.Object, recs, "status", "recommendations")

		_, err = w.dynClient.Resource(classRes).UpdateStatus(ctx, latest, metav1.UpdateOptions{})
		return err
	})
}

func hasPendingRecommendation(recs []any) bool {
	for _, rec := range recs {
		m, ok := rec.(map[string]any)
		if !ok {
			continue
		}
		status, _ := m["status"].(string)
		if strings.EqualFold(status, "Pending") || status == "" {
			return true
		}
	}
	return false
}

func toResourceRefKeys(resources []any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, r := range resources {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := m["kind"].(string)
		md, _ := m["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		if kind != "" && name != "" {
			out[kind+"|"+name] = struct{}{}
		}
	}
	return out
}

type templateTarget struct {
	apiVersion string
	kind       string
	name       string
	resource   schema.GroupVersionResource
	namespaced bool
	compareOn  string
	expected   any
	extractor  compareExtractor
}

func (w *Watcher) resolveTemplateTarget(raw map[string]any) (*templateTarget, error) {
	u := &unstructured.Unstructured{Object: raw}
	gvk := u.GroupVersionKind()
	mapping, err := w.mapper.RESTMapping(schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}, gvk.Version)
	if err != nil {
		return nil, err
	}
	strategy := strategyForKind(u.GetKind())
	expected := strategy.extractor(u.Object)
	return &templateTarget{
		apiVersion: u.GetAPIVersion(),
		kind:       u.GetKind(),
		name:       u.GetName(),
		resource:   mapping.Resource,
		namespaced: mapping.Scope.Name() == "namespace",
		compareOn:  strategy.name,
		expected:   expected,
		extractor:  strategy.extractor,
	}, nil
}

func buildTemplateTargetsForWatcher(w *Watcher, resources []any) ([]templateTarget, error) {
	out := make([]templateTarget, 0, len(resources))
	for _, r := range resources {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		t, err := w.resolveTemplateTarget(m)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

func normalizeJSONValue(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func extractDataField(obj map[string]any) any {
	out := map[string]any{}
	if data, ok := obj["data"].(map[string]any); ok {
		for k, v := range data {
			out[k] = v
		}
	}
	if binaryData, ok := obj["binaryData"].(map[string]any); ok {
		out["binaryData"] = binaryData
	}
	return out
}

func extractRulesField(obj map[string]any) any {
	if rules, ok := obj["rules"]; ok {
		return normalizeRules(rules)
	}
	return []any{}
}

func extractWebhooksField(obj map[string]any) any {
	if webhooks, ok := obj["webhooks"]; ok {
		return normalizeWebhookConfigs(webhooks)
	}
	return []any{}
}

func extractSpecField(obj map[string]any) any {
	if spec, ok := obj["spec"]; ok {
		return spec
	}
	return map[string]any{}
}

// extractNetworkPolicySpec compares meaningful NetworkPolicy fields with stable ordering
// so AI drift detection matches the namespace controller’s templates across rule order churn.
func extractNetworkPolicySpec(obj map[string]any) any {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if v, ok := spec["podSelector"]; ok {
		out["podSelector"] = v
	}
	if v, ok := spec["policyTypes"]; ok {
		out["policyTypes"] = normalizeStringSlice(v)
	}
	if v, ok := spec["ingress"]; ok {
		out["ingress"] = sortJSONStableSlice(v)
	}
	if v, ok := spec["egress"]; ok {
		out["egress"] = sortJSONStableSlice(v)
	}
	return out
}

func sortJSONStableSlice(v any) any {
	items, ok := v.([]any)
	if !ok {
		return []any{}
	}
	type pair struct {
		key string
		val any
	}
	pairs := make([]pair, 0, len(items))
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		pairs = append(pairs, pair{key: string(b), val: item})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	out := make([]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.val)
	}
	return out
}

func normalizeWebhookConfigs(v any) any {
	items, ok := v.([]any)
	if !ok {
		return []any{}
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		x := map[string]any{}
		for _, key := range []string{
			"name", "admissionReviewVersions", "sideEffects", "failurePolicy",
			"matchPolicy", "timeoutSeconds", "rules", "namespaceSelector", "objectSelector",
			"reinvocationPolicy",
		} {
			if val, exists := m[key]; exists {
				x[key] = val
			}
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := out[i]["name"].(string)
		nj, _ := out[j]["name"].(string)
		return ni < nj
	})
	return out
}

func normalizeServiceAccountFields(obj map[string]any) any {
	out := map[string]any{}
	if v, ok := obj["automountServiceAccountToken"]; ok {
		out["automountServiceAccountToken"] = v
	}
	if v, ok := obj["imagePullSecrets"]; ok {
		out["imagePullSecrets"] = normalizeNamedRefs(v)
	} else {
		out["imagePullSecrets"] = []any{}
	}
	if v, ok := obj["secrets"]; ok {
		out["secrets"] = normalizeNamedRefs(v)
	} else {
		out["secrets"] = []any{}
	}
	return out
}

func normalizeNamedRefs(v any) any {
	items, ok := v.([]any)
	if !ok {
		return []any{}
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	return out
}

func normalizeResourceQuotaFields(obj map[string]any) any {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if hard, ok := spec["hard"]; ok {
		out["hard"] = hard
	}
	if scopes, ok := spec["scopes"]; ok {
		out["scopes"] = normalizeStringSlice(scopes)
	}
	if scopeSelector, ok := spec["scopeSelector"]; ok {
		out["scopeSelector"] = scopeSelector
	}
	return out
}

func normalizeLimitRangeFields(obj map[string]any) any {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	limits, _ := spec["limits"].([]any)
	normalized := make([]map[string]any, 0, len(limits))
	for _, item := range limits {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		x := map[string]any{}
		for _, key := range []string{"type", "max", "min", "default", "defaultRequest", "maxLimitRequestRatio"} {
			if val, exists := m[key]; exists {
				x[key] = val
			}
		}
		normalized = append(normalized, x)
	}
	sort.Slice(normalized, func(i, j int) bool {
		ti, _ := normalized[i]["type"].(string)
		tj, _ := normalized[j]["type"].(string)
		return ti < tj
	})
	out["limits"] = normalized
	return out
}

func normalizeStringSlice(v any) any {
	items, ok := v.([]any)
	if !ok {
		return []any{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	asAny := make([]any, 0, len(out))
	for _, s := range out {
		asAny = append(asAny, s)
	}
	return asAny
}

func normalizeRules(v any) any {
	rules, ok := v.([]any)
	if !ok {
		return []any{}
	}
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		x := map[string]any{}
		for _, key := range []string{"apiGroups", "resources", "resourceNames", "verbs", "nonResourceURLs"} {
			if val, exists := m[key]; exists {
				x[key] = val
			}
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprintf("%v", out[i]) < fmt.Sprintf("%v", out[j])
	})
	return out
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var _ managerRunnable = (*Watcher)(nil)

type managerRunnable interface {
	Start(context.Context) error
	NeedLeaderElection() bool
}
