package controller

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openinspectv1alpha1 "github.com/dirien/openinspect-operator/api/v1alpha1"
)

// ensureWeb runs the web UI in-cluster as an owned Deployment + Service.
// Owner references make cleanup automatic on CR deletion; removal from spec
// is handled explicitly below.
func (r *OpenInspectReconciler) ensureWeb(ctx context.Context, oi *openinspectv1alpha1.OpenInspect) error {
	name := oi.Name + "-web"
	if oi.Spec.Web == nil {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}},
		} {
			if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
				return fmt.Errorf("remove web %T: %w", obj, err)
			}
		}
		meta.RemoveStatusCondition(&oi.Status.Conditions, openinspectv1alpha1.ConditionWeb)
		return nil
	}
	web := oi.Spec.Web
	labels := map[string]string{
		"app.kubernetes.io/name":       "openinspect-web",
		"app.kubernetes.io/instance":   oi.Name,
		"app.kubernetes.io/managed-by": "openinspect-operator",
	}
	port := web.Port
	if port == 0 {
		port = 3000
	}

	env := make([]corev1.EnvVar, 0, len(web.Env)+len(web.SecretEnv))
	for _, k := range sortedKeys(web.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: web.Env[k]})
	}
	for _, s := range web.SecretEnv {
		env = append(env, corev1.EnvVar{
			Name: s.Binding,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.SecretRef.Name},
					Key:                  s.SecretRef.Key,
				},
			},
		})
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = web.Replicas
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deploy.Spec.Template.Labels = labels
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "web",
			Image: web.Image,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
			Env:   env,
		}}
		return ctrl.SetControllerReference(oi, deploy, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("web deployment: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oi.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = labels
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: "http", Port: 80, TargetPort: intstr.FromInt32(port),
		}}
		return ctrl.SetControllerReference(oi, svc, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("web service: %w", err)
	}

	ready := deploy.Status.ReadyReplicas
	r.setCondition(oi, openinspectv1alpha1.ConditionWeb, metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("web deployment applied, %d replicas ready", ready))
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
