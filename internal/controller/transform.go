package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
)

// PodTransform returns a cache transform that keeps only the pod fields the
// controller reads, so caching every pod in the cluster stays cheap.
func PodTransform(a Annotations) toolscache.TransformFunc {
	return func(obj any) (any, error) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return obj, nil
		}
		stripped := &corev1.Pod{
			TypeMeta: pod.TypeMeta,
			ObjectMeta: metav1.ObjectMeta{
				Name:              pod.Name,
				Namespace:         pod.Namespace,
				UID:               pod.UID,
				ResourceVersion:   pod.ResourceVersion,
				CreationTimestamp: pod.CreationTimestamp,
				DeletionTimestamp: pod.DeletionTimestamp,
			},
			Spec:   corev1.PodSpec{NodeName: pod.Spec.NodeName},
			Status: corev1.PodStatus{Phase: pod.Status.Phase},
		}
		if v, ok := pod.Annotations[a.PTR]; ok {
			stripped.Annotations = map[string]string{a.PTR: v}
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				stripped.Status.Conditions = []corev1.PodCondition{{Type: c.Type, Status: c.Status}}
				break
			}
		}
		return stripped, nil
	}
}
