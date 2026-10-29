package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodTransform(t *testing.T) {
	a := AnnotationsWithPrefix(DefaultAnnotationPrefix)
	p := pod("smtp", "mail.example.com", time.Now(), true)
	p.Labels = map[string]string{"app": "smtp"}
	p.Annotations["other"] = "x"
	p.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}
	p.Spec.Containers = []corev1.Container{{Name: "smtp", Image: "smtp:1"}}
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue})

	obj, err := PodTransform(a)(p)
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*corev1.Pod)

	if got.Labels != nil || got.ManagedFields != nil || got.Spec.Containers != nil {
		t.Fatalf("unused fields kept: %+v", got)
	}
	if len(got.Annotations) != 1 || len(got.Status.Conditions) != 1 {
		t.Fatalf("annotations = %v, conditions = %v", got.Annotations, got.Status.Conditions)
	}
	if got.Spec.NodeName != "node-1" || !podActive(got) {
		t.Fatal("stripped pod lost fields the controller reads")
	}

	r := &NodeReconciler{Annotations: a}
	if ptr, source, _ := r.desiredFromPods([]corev1.Pod{*got}); ptr != "mail.example.com" || source != "mail/smtp" {
		t.Fatalf("desired = %q from %q", ptr, source)
	}
}
