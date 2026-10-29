package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const nodeIP = "203.0.113.10"

type fakeRobot struct {
	ptrs   map[string]string
	writes int
	reads  int
}

func (f *fakeRobot) GetPTR(_ context.Context, ip string) (string, error) {
	f.reads++
	return f.ptrs[ip], nil
}

func (f *fakeRobot) SetPTR(_ context.Context, ip, ptr string) error {
	f.writes++
	f.ptrs[ip] = ptr
	return nil
}

func (f *fakeRobot) DeletePTR(_ context.Context, ip string) error {
	f.writes++
	delete(f.ptrs, ip)
	return nil
}

type fixture struct {
	r     *NodeReconciler
	robot *fakeRobot
	rec   *events.FakeRecorder
	now   time.Time
}

func newFixture(t *testing.T, objs ...client.Object) *fixture {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(obj client.Object) []string {
			return []string{obj.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
	f := &fixture{
		robot: &fakeRobot{ptrs: map[string]string{nodeIP: "static.10.113.0.203.clients.your-server.de"}},
		rec:   events.NewFakeRecorder(10),
		now:   time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
	}
	f.r = &NodeReconciler{
		Client:         c,
		Robot:          f.robot,
		Recorder:       f.rec,
		Annotations:    AnnotationsWithPrefix(DefaultAnnotationPrefix),
		ResetDelay:     time.Minute,
		ResyncInterval: 15 * time.Minute,
		Now:            func() time.Time { return f.now },
	}
	return f
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-1"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func node(annotations map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Annotations: annotations},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
			{Type: corev1.NodeExternalIP, Address: nodeIP},
		}},
	}
}

func pod(name, ptr string, created time.Time, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "mail",
			CreationTimestamp: metav1.NewTime(created),
			Annotations:       map[string]string{DefaultAnnotationPrefix + "/ptr": ptr},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func TestSetsPTRFromPod(t *testing.T) {
	created := time.Now()
	f := newFixture(t, node(nil), pod("smtp", "Mail.Example.com.", created, true))

	res := f.reconcile(t)
	if got := f.robot.ptrs[nodeIP]; got != "mail.example.com" {
		t.Fatalf("ptr = %q", got)
	}
	if res.RequeueAfter != 15*time.Minute {
		t.Fatalf("requeueAfter = %v", res.RequeueAfter)
	}

	// A second reconcile within the resync interval must not hit the API.
	reads := f.robot.reads
	f.reconcile(t)
	if f.robot.reads != reads || f.robot.writes != 1 {
		t.Fatalf("expected cached result, reads=%d writes=%d", f.robot.reads, f.robot.writes)
	}
}

func TestIgnoresUnreadyPod(t *testing.T) {
	f := newFixture(t, node(nil), pod("smtp", "mail.example.com", time.Now(), false))
	f.reconcile(t)
	// Hetzner's generated name already is the default, so nothing is written.
	if f.robot.writes != 0 {
		t.Fatalf("expected no write, got %d (ptr %q)", f.robot.writes, f.robot.ptrs[nodeIP])
	}
}

func TestResetsAfterDelay(t *testing.T) {
	p := pod("smtp", "mail.example.com", time.Now(), true)
	f := newFixture(t, node(map[string]string{DefaultAnnotationPrefix + "/default-ptr": "node-1.example.com"}), p)
	f.reconcile(t)

	if err := f.r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10 * time.Second)
	if res := f.reconcile(t); res.RequeueAfter != 50*time.Second {
		t.Fatalf("expected delayed reset, requeueAfter = %v", res.RequeueAfter)
	}
	if got := f.robot.ptrs[nodeIP]; got != "mail.example.com" {
		t.Fatalf("ptr reset too early: %q", got)
	}

	f.now = f.now.Add(time.Minute)
	f.reconcile(t)
	if got := f.robot.ptrs[nodeIP]; got != "node-1.example.com" {
		t.Fatalf("ptr = %q, want node default", got)
	}
}

func TestConflictPicksOldest(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	f := newFixture(t, node(nil),
		pod("newer", "b.example.com", base.Add(time.Hour), true),
		pod("older", "a.example.com", base, true),
	)
	f.reconcile(t)
	if got := f.robot.ptrs[nodeIP]; got != "a.example.com" {
		t.Fatalf("ptr = %q", got)
	}
	if ev := <-f.rec.Events; !strings.HasPrefix(ev, "Warning ConflictingPTR") {
		t.Fatalf("expected conflict event, got %q", ev)
	}
	<-f.rec.Events // PTRUpdated

	f.reconcile(t)
	if n := len(f.rec.Events); n != 0 {
		t.Fatalf("conflict event repeated, %d new events", n)
	}

	third := pod("third", "c.example.com", base.Add(2*time.Hour), true)
	if err := f.r.Create(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if ev := <-f.rec.Events; !strings.Contains(ev, "c.example.com") {
		t.Fatalf("expected updated conflict event, got %q", ev)
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	f := newFixture(t, node(nil), pod("smtp", "mail.example.com", time.Now(), true))
	f.r.DryRun = true
	f.reconcile(t)
	if f.robot.writes != 0 {
		t.Fatalf("dry-run wrote %d times", f.robot.writes)
	}
}

func TestSkipsNodeWithoutPublicIP(t *testing.T) {
	n := node(nil)
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}}
	f := newFixture(t, n, pod("smtp", "mail.example.com", time.Now(), true))
	f.reconcile(t)
	if f.robot.reads != 0 {
		t.Fatal("robot api called for node without public ip")
	}
}

func TestIPAnnotationOverride(t *testing.T) {
	f := newFixture(t, node(map[string]string{DefaultAnnotationPrefix + "/ip": "198.51.100.7"}),
		pod("smtp", "mail.example.com", time.Now(), true))
	f.reconcile(t)
	if got := f.robot.ptrs["198.51.100.7"]; got != "mail.example.com" {
		t.Fatalf("ptr = %q", got)
	}
}
