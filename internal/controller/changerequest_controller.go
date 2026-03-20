package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type NamespaceClassChangeRequestReconciler struct {
	client.Client
}

func (r *NamespaceClassChangeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "akuity.io/v1alpha1",
			"kind":       "NamespaceClassChangeRequest",
		}}).
		Complete(r)
}

func (r *NamespaceClassChangeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("changeRequest", req.Name)
	cr, err := r.getChangeRequest(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	approved, _, _ := unstructured.NestedBool(cr.Object, "spec", "approved")
	if !approved {
		return ctrl.Result{}, r.updateChangeRequestStatus(ctx, req.NamespacedName, "Pending", "Waiting for approval", nil)
	}

	className, _, _ := unstructured.NestedString(cr.Object, "spec", "namespaceClassName")
	recommendationID, _, _ := unstructured.NestedString(cr.Object, "spec", "recommendationID")
	allowHighRisk, _, _ := unstructured.NestedBool(cr.Object, "spec", "allowHighRisk")
	if className == "" || recommendationID == "" {
		return ctrl.Result{}, r.updateChangeRequestStatus(
			ctx,
			req.NamespacedName,
			"Rejected",
			"spec.namespaceClassName and spec.recommendationID are required",
			nil,
		)
	}

	classObj := &unstructured.Unstructured{}
	classObj.SetAPIVersion("akuity.io/v1alpha1")
	classObj.SetKind("NamespaceClass")
	if err := r.Get(ctx, types.NamespacedName{Name: className}, classObj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.updateChangeRequestStatus(ctx, req.NamespacedName, "Rejected", "NamespaceClass not found", nil)
		}
		return ctrl.Result{}, err
	}

	recs, _, _ := unstructured.NestedSlice(classObj.Object, "status", "recommendations")
	idx := -1
	var proposed []any
	for i, rec := range recs {
		recMap, ok := rec.(map[string]any)
		if !ok {
			continue
		}
		id, _ := recMap["id"].(string)
		if id != recommendationID {
			continue
		}
		idx = i
		if p, ok := recMap["proposedResources"].([]any); ok {
			proposed = p
		}
		break
	}
	if idx < 0 {
		return ctrl.Result{}, r.updateChangeRequestStatus(
			ctx,
			req.NamespacedName,
			"Rejected",
			"recommendationID not found on NamespaceClass status",
			nil,
		)
	}
	if len(proposed) == 0 {
		return ctrl.Result{}, r.updateChangeRequestStatus(
			ctx,
			req.NamespacedName,
			"Rejected",
			"recommendation has empty proposedResources",
			nil,
		)
	}

	if !allowHighRisk {
		for _, p := range proposed {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			u := &unstructured.Unstructured{Object: m}
			if isHighRiskResource(u) {
				return ctrl.Result{}, r.updateChangeRequestStatus(
					ctx,
					req.NamespacedName,
					"Rejected",
					fmt.Sprintf("high-risk resource requires spec.allowHighRisk=true (%s)", u.GetKind()),
					nil,
				)
			}
		}
	}

	specMap, _, _ := unstructured.NestedMap(classObj.Object, "spec")
	specMap["resources"] = proposed
	if err := unstructured.SetNestedMap(classObj.Object, specMap, "spec"); err != nil {
		return ctrl.Result{}, err
	}
	ann := classObj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	if allowHighRisk {
		ann["namespaceclass.akuity.io/allow-high-risk"] = "true"
	}
	classObj.SetAnnotations(ann)
	if err := r.Update(ctx, classObj); err != nil {
		return ctrl.Result{}, err
	}
	rolledBack, err := r.rollbackNamespacesForSwitch(ctx, className)
	if err != nil {
		return ctrl.Result{}, err
	}

	recMap := recs[idx].(map[string]any)
	recMap["status"] = "Applied"
	recMap["appliedAt"] = metav1.Now().Format(time.RFC3339)
	recs[idx] = recMap
	_ = unstructured.SetNestedSlice(classObj.Object, recs, "status", "recommendations")
	_ = unstructured.SetNestedSlice(classObj.Object, []any{
		map[string]any{
			"type":               "RecommendationPending",
			"status":             "False",
			"reason":             "ApprovedAndApplied",
			"message":            "Approved recommendation applied",
			"lastTransitionTime": metav1.Now().Format(time.RFC3339),
		},
	}, "status", "conditions")
	if err := r.updateNamespaceClassStatus(ctx, className, recMap, idx); err != nil {
		logger.Error(err, "status update failed")
	}

	b, _ := json.Marshal(proposed)
	snapshot := string(b)
	return ctrl.Result{}, r.updateChangeRequestStatus(
		ctx,
		req.NamespacedName,
		"Applied",
		fmt.Sprintf("Applied recommendation %s to NamespaceClass %s (rolled back %d namespace switch(es))", recommendationID, className, rolledBack),
		&snapshot,
	)
}

func (r *NamespaceClassChangeRequestReconciler) getChangeRequest(ctx context.Context, key types.NamespacedName) (*unstructured.Unstructured, error) {
	cr := &unstructured.Unstructured{}
	cr.SetAPIVersion("akuity.io/v1alpha1")
	cr.SetKind("NamespaceClassChangeRequest")
	if err := r.Get(ctx, key, cr); err != nil {
		return nil, err
	}
	return cr, nil
}

func (r *NamespaceClassChangeRequestReconciler) updateChangeRequestStatus(
	ctx context.Context,
	key types.NamespacedName,
	phase string,
	message string,
	appliedSpecSnapshot *string,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cr, err := r.getChangeRequest(ctx, key)
		if err != nil {
			return err
		}
		_ = unstructured.SetNestedField(cr.Object, phase, "status", "phase")
		_ = unstructured.SetNestedField(cr.Object, message, "status", "message")
		_ = unstructured.SetNestedField(cr.Object, metav1.Now().Format(time.RFC3339), "status", "lastTransitionTime")
		if appliedSpecSnapshot != nil {
			_ = unstructured.SetNestedField(cr.Object, *appliedSpecSnapshot, "status", "appliedSpecSnapshot")
		}
		return r.Status().Update(ctx, cr)
	})
}

func (r *NamespaceClassChangeRequestReconciler) updateNamespaceClassStatus(
	ctx context.Context,
	className string,
	appliedRec map[string]any,
	recIdx int,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		classObj := &unstructured.Unstructured{}
		classObj.SetAPIVersion("akuity.io/v1alpha1")
		classObj.SetKind("NamespaceClass")
		if err := r.Get(ctx, types.NamespacedName{Name: className}, classObj); err != nil {
			return err
		}
		recs, _, _ := unstructured.NestedSlice(classObj.Object, "status", "recommendations")
		if recIdx < len(recs) {
			recs[recIdx] = appliedRec
		}
		_ = unstructured.SetNestedSlice(classObj.Object, recs, "status", "recommendations")
		_ = unstructured.SetNestedSlice(classObj.Object, []any{
			map[string]any{
				"type":               "RecommendationPending",
				"status":             "False",
				"reason":             "ApprovedAndApplied",
				"message":            "Approved recommendation applied",
				"lastTransitionTime": metav1.Now().Format(time.RFC3339),
			},
		}, "status", "conditions")
		return r.Status().Update(ctx, classObj)
	})
}

func (r *NamespaceClassChangeRequestReconciler) rollbackNamespacesForSwitch(ctx context.Context, className string) (int, error) {
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces, client.MatchingLabels{ClassLabelKey: className}); err != nil {
		return 0, err
	}
	updated := 0
	for i := range namespaces.Items {
		ns := &namespaces.Items[i]
		ann := ns.GetAnnotations()
		if ann == nil {
			continue
		}
		prevClass := ann[PrevClassAnnoKey]
		if prevClass == "" || prevClass == className {
			continue
		}
		labels := ns.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ClassLabelKey] = prevClass
		ns.SetLabels(labels)

		ann[LastClassAnnoKey] = prevClass
		delete(ann, PrevClassAnnoKey)
		delete(ann, SwitchedAtAnnoKey)
		ns.SetAnnotations(ann)

		if err := r.Update(ctx, ns); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}
