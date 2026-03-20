package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ClassLabelKey       = "namespaceclass.akuity.io/name"
	ManagedLabelKey     = "namespaceclass.akuity.io/managed"
	ManagedNSLabelKey   = "namespaceclass.akuity.io/namespace"
	ManagedClassLabel   = "namespaceclass.akuity.io/class"
	ManagedHashLabel    = "namespaceclass.akuity.io/template-hash"
	InventoryConfigName = "namespaceclass-inventory"
	LastClassAnnoKey    = "namespaceclass.akuity.io/last-class"
	PrevClassAnnoKey    = "namespaceclass.akuity.io/previous-class"
	SwitchedAtAnnoKey   = "namespaceclass.akuity.io/switched-at"
)

type NamespaceClassReconciler struct {
	client.Client
	dynClient dynamic.Interface
	mapper    *restmapper.DeferredDiscoveryRESTMapper
}

type namespaceClassSpec struct {
	Resources []json.RawMessage `json:"resources,omitempty"`
}

type resourceRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

func (r *NamespaceClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	cfg := mgr.GetConfig()

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	r.dynClient = dynClient
	r.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(cfg)))

	namespacePred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldClass := e.ObjectOld.GetLabels()[ClassLabelKey]
			newClass := e.ObjectNew.GetLabels()[ClassLabelKey]
			return oldClass != newClass || e.ObjectOld.GetDeletionTimestamp() != e.ObjectNew.GetDeletionTimestamp()
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
	}
	classPred := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Reconcile namespaces only when NamespaceClass spec changes.
			// Status-only updates from watcher should not auto-heal resources immediately,
			// otherwise drift may disappear before recommendation flow can be exercised.
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return true },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}, builder.WithPredicates(namespacePred)).
		Watches(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "akuity.io/v1alpha1",
			"kind":       "NamespaceClass",
		}}, handler.EnqueueRequestsFromMapFunc(r.mapClassToNamespaces), builder.WithPredicates(classPred)).
		Complete(r)
}

func (r *NamespaceClassReconciler) mapClassToNamespaces(ctx context.Context, obj client.Object) []reconcile.Request {
	className := obj.GetName()
	var nsl corev1.NamespaceList
	if err := r.List(ctx, &nsl, client.MatchingLabels{ClassLabelKey: className}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(nsl.Items))
	for i := range nsl.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: nsl.Items[i].Name}})
	}
	return reqs
}

func (r *NamespaceClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Name)

	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desiredClass := ns.Labels[ClassLabelKey]
	desiredRefs := []resourceRef{}

	if desiredClass != "" {
		classSpec, allowHighRisk, err := r.getNamespaceClassSpec(ctx, desiredClass)
		if err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("namespace references missing NamespaceClass; deleting managed resources", "class", desiredClass)
			} else {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		} else {
			desiredRefs, err = r.applyClassResources(ctx, &ns, desiredClass, classSpec.Resources, allowHighRisk)
			if err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	}

	prevRefs, err := r.readInventory(ctx, ns.Name)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	if err := r.deleteRemovedResources(ctx, ns.Name, prevRefs, desiredRefs); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	if err := r.writeInventory(ctx, ns.Name, desiredRefs); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	if err := r.updateSwitchAnnotations(ctx, &ns); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	return ctrl.Result{}, nil
}

func (r *NamespaceClassReconciler) updateSwitchAnnotations(ctx context.Context, ns *corev1.Namespace) error {
	desiredClass := ns.Labels[ClassLabelKey]
	ann := ns.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	lastClass := ann[LastClassAnnoKey]
	changed := false

	// Record a switch event whenever desired class changes from previously applied class.
	if lastClass != "" && lastClass != desiredClass {
		ann[PrevClassAnnoKey] = lastClass
		ann[SwitchedAtAnnoKey] = time.Now().Format(time.RFC3339)
		changed = true
	}
	if ann[LastClassAnnoKey] != desiredClass {
		ann[LastClassAnnoKey] = desiredClass
		changed = true
	}
	if !changed {
		return nil
	}
	ns.SetAnnotations(ann)
	return r.Update(ctx, ns)
}

func (r *NamespaceClassReconciler) getNamespaceClassSpec(ctx context.Context, className string) (*namespaceClassSpec, bool, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("akuity.io/v1alpha1")
	u.SetKind("NamespaceClass")
	if err := r.Get(ctx, types.NamespacedName{Name: className}, u); err != nil {
		return nil, false, err
	}
	allowHighRisk := u.GetAnnotations()["namespaceclass.akuity.io/allow-high-risk"] == "true"
	specMap, found, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil || !found {
		return &namespaceClassSpec{}, allowHighRisk, err
	}
	specRaw, err := json.Marshal(specMap)
	if err != nil {
		return nil, false, err
	}
	var spec namespaceClassSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return nil, false, err
	}
	return &spec, allowHighRisk, nil
}

func (r *NamespaceClassReconciler) applyClassResources(ctx context.Context, ns *corev1.Namespace, className string, resources []json.RawMessage, allowHighRisk bool) ([]resourceRef, error) {
	refs := make([]resourceRef, 0, len(resources))
	for _, resRaw := range resources {
		var obj map[string]any
		if err := json.Unmarshal(resRaw, &obj); err != nil {
			return nil, fmt.Errorf("invalid resource template JSON: %w", err)
		}
		u := &unstructured.Unstructured{Object: obj}
		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			return nil, fmt.Errorf("resource template missing apiVersion or kind")
		}
		if u.GetName() == "" {
			return nil, fmt.Errorf("resource template %s/%s missing metadata.name", u.GetAPIVersion(), u.GetKind())
		}
		if isHighRiskResource(u) && !allowHighRisk {
			return nil, fmt.Errorf("high-risk resource blocked by policy: %s %s", u.GetKind(), u.GetName())
		}

		gvk := u.GroupVersionKind()
		mapping, err := r.mapper.RESTMapping(schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}, gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("unable to map GVK %s: %w", gvk.String(), err)
		}

		if mapping.Scope.Name() == "namespace" {
			u.SetNamespace(ns.Name)
		}

		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ManagedLabelKey] = "true"
		labels[ManagedNSLabelKey] = ns.Name
		labels[ManagedClassLabel] = className
		labels[ManagedHashLabel] = hashTemplate(resRaw)
		u.SetLabels(labels)

		resourceClient := r.dynClient.Resource(mapping.Resource)
		var writeClient dynamic.ResourceInterface = resourceClient
		if mapping.Scope.Name() == "namespace" {
			writeClient = resourceClient.Namespace(ns.Name)
		}

		existing, err := writeClient.Get(ctx, u.GetName(), metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if apierrors.IsNotFound(err) {
			_, err = writeClient.Create(ctx, u, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				// Another reconcile may have created it between Get and Create.
				latest, getErr := writeClient.Get(ctx, u.GetName(), metav1.GetOptions{})
				if getErr != nil {
					return nil, getErr
				}
				u.SetResourceVersion(latest.GetResourceVersion())
				_, err = writeClient.Update(ctx, u, metav1.UpdateOptions{})
			}
		} else {
			u.SetResourceVersion(existing.GetResourceVersion())
			_, err = writeClient.Update(ctx, u, metav1.UpdateOptions{})
		}
		if err != nil {
			return nil, fmt.Errorf("apply failed for %s/%s: %w", gvk.String(), u.GetName(), err)
		}

		refs = append(refs, resourceRef{
			APIVersion: u.GetAPIVersion(),
			Kind:       u.GetKind(),
			Name:       u.GetName(),
			Namespace:  u.GetNamespace(),
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].refKey() < refs[j].refKey()
	})
	return refs, nil
}

func (r *NamespaceClassReconciler) deleteRemovedResources(ctx context.Context, namespace string, oldRefs, newRefs []resourceRef) error {
	desired := map[string]struct{}{}
	for _, ref := range newRefs {
		desired[ref.refKey()] = struct{}{}
	}

	for _, old := range oldRefs {
		if _, exists := desired[old.refKey()]; exists {
			continue
		}
		gv, err := schema.ParseGroupVersion(old.APIVersion)
		if err != nil {
			continue
		}
		mapping, err := r.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: old.Kind}, gv.Version)
		if err != nil {
			continue
		}
		resourceClient := r.dynClient.Resource(mapping.Resource)
		var deleteClient dynamic.ResourceInterface = resourceClient
		targetNS := old.Namespace
		if mapping.Scope.Name() == "namespace" {
			if targetNS == "" {
				targetNS = namespace
			}
			deleteClient = resourceClient.Namespace(targetNS)
		}
		err = deleteClient.Delete(ctx, old.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *NamespaceClassReconciler) readInventory(ctx context.Context, namespace string) ([]resourceRef, error) {
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: InventoryConfigName}, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	raw := cm.Data["resources"]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var refs []resourceRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

func (r *NamespaceClassReconciler) writeInventory(ctx context.Context, namespace string, refs []resourceRef) error {
	raw, err := json.Marshal(refs)
	if err != nil {
		return err
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: namespace, Name: InventoryConfigName}
	if err := r.Get(ctx, key, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		newCM := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      InventoryConfigName,
				Namespace: namespace,
				Labels: map[string]string{
					ManagedLabelKey: "true",
				},
			},
			Data: map[string]string{"resources": string(raw)},
		}
		return r.Create(ctx, &newCM)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["resources"] = string(raw)
	return r.Update(ctx, &cm)
}

func (r resourceRef) refKey() string {
	return fmt.Sprintf("%s|%s|%s|%s", r.APIVersion, r.Kind, r.Namespace, r.Name)
}

func hashTemplate(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func isHighRiskResource(u *unstructured.Unstructured) bool {
	kind := strings.ToLower(u.GetKind())
	if kind == "role" || kind == "clusterrole" || kind == "customresourcedefinition" {
		return true
	}
	return strings.Contains(kind, "webhook")
}
