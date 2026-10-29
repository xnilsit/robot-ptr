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

	"github.com/xnilsit/robot-ptr/internal/robot"
)

const nodeIP = "203.0.113.10"

type fakeRobot struct {
	ptrs    map[string]string
	foreign map[string]bool // IPs that do not belong to the account
	writes  int
	reads   int
}

var errIPNotFound = &robot.APIError{Status: 404, Code: "IP_NOT_FOUND", Message: "The IP address is not found"}

func (f *fakeRobot) GetPTR(_ context.Context, ip string) (string, error) {
	f.reads++
	if f.foreign[ip] {
		return "", errIPNotFound
	}
	return f.ptrs[ip], nil
}

func (f *fakeRobot) SetPTR(_ context.Context, ip, ptr string) error {
	f.writes++
	if f.foreign[ip] {
		return errIPNotFound
	}
	f.ptrs[ip] = ptr
	return nil
}

func (f *fakeRobot) DeletePTR(_ context.Context, ip string) error {
	f.writes++
	if f.foreign[ip] {
		return errIPNotFound
	}
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

// restart replaces the reconciler, dropping all in-memory state.
func (f *fixture) restart() {
	r := f.r
	f.r = &NodeReconciler{
		Client:         r.Client,
		Robot:          r.Robot,
		Recorder:       r.Recorder,
		Annotations:    r.Annotations,
		ResetDelay:     r.ResetDelay,
		ResyncInterval: r.ResyncInterval,
		Now:            r.Now,
	}
}

func (f *fixture) podLeftAt(t *testing.T) string {
	t.Helper()
	var n corev1.Node
	if err := f.r.Get(context.Background(), types.NamespacedName{Name: "node-1"}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Annotations[f.r.Annotations.PodLeftAt]
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
	if res := f.reconcile(t); res.RequeueAfter != time.Minute {
		t.Fatalf("expected delayed reset, requeueAfter = %v", res.RequeueAfter)
	}
	if got := f.podLeftAt(t); got != "2026-10-29T12:00:00Z" {
		t.Fatalf("pod-left-at = %q", got)
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
	if got := f.podLeftAt(t); got != "" {
		t.Fatalf("pod-left-at not removed after reset: %q", got)
	}
}

func TestResetDelaySurvivesRestart(t *testing.T) {
	p := pod("smtp", "mail.example.com", time.Now(), true)
	f := newFixture(t, node(nil), p)
	f.reconcile(t)
	if err := f.r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)

	f.restart()
	f.now = f.now.Add(20 * time.Second)
	if res := f.reconcile(t); res.RequeueAfter != 40*time.Second {
		t.Fatalf("expected delayed reset after restart, requeueAfter = %v", res.RequeueAfter)
	}
	if f.robot.writes != 1 {
		t.Fatalf("ptr reset too early: %q", f.robot.ptrs[nodeIP])
	}
}

func TestRestartWithoutPodDelaysReset(t *testing.T) {
	// The previous instance set the PTR but stopped before seeing the pod leave.
	f := newFixture(t, node(nil))
	f.robot.ptrs[nodeIP] = "mail.example.com"

	if res := f.reconcile(t); res.RequeueAfter != time.Minute {
		t.Fatalf("expected delayed reset, requeueAfter = %v", res.RequeueAfter)
	}
	if f.robot.writes != 0 || f.podLeftAt(t) == "" {
		t.Fatalf("expected pod-left-at and no write, writes=%d", f.robot.writes)
	}
}

func TestReturningPodCancelsReset(t *testing.T) {
	f := newFixture(t, node(map[string]string{DefaultAnnotationPrefix + "/pod-left-at": "2026-10-29T11:59:30Z"}),
		pod("smtp", "mail.example.com", time.Now(), true))
	f.robot.ptrs[nodeIP] = "mail.example.com"

	f.reconcile(t)
	if got := f.podLeftAt(t); got != "" {
		t.Fatalf("pod-left-at not removed: %q", got)
	}
	if f.robot.writes != 0 {
		t.Fatalf("unexpected write: %q", f.robot.ptrs[nodeIP])
	}
}

func TestNoDelayWhenAlreadyDefault(t *testing.T) {
	f := newFixture(t, node(nil))
	if res := f.reconcile(t); res.RequeueAfter != 15*time.Minute {
		t.Fatalf("requeueAfter = %v", res.RequeueAfter)
	}
	if got := f.podLeftAt(t); got != "" {
		t.Fatalf("unexpected pod-left-at %q", got)
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

func TestForeignIPIsNotRetried(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"pod ptr":     {node(nil), pod("smtp", "mail.example.com", time.Now(), true)},
		"default ptr": {node(map[string]string{DefaultAnnotationPrefix + "/default-ptr": "node-1.example.com"})},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, objs...)
			f.robot.foreign = map[string]bool{nodeIP: true}

			if res := f.reconcile(t); res.RequeueAfter != 15*time.Minute {
				t.Fatalf("requeueAfter = %v", res.RequeueAfter)
			}
			if f.robot.writes != 0 {
				t.Fatalf("unexpected write")
			}
			if ev := <-f.rec.Events; !strings.HasPrefix(ev, "Warning NotRobotIP") {
				t.Fatalf("expected NotRobotIP event, got %q", ev)
			}

			// The result is cached like a PTR, so the IP is not re-read before
			// the next resync.
			f.reconcile(t)
			if f.robot.reads != 1 {
				t.Fatalf("reads = %d", f.robot.reads)
			}
			f.now = f.now.Add(15 * time.Minute)
			f.reconcile(t)
			if f.robot.reads != 2 {
				t.Fatalf("reads after resync = %d", f.robot.reads)
			}
			if n := len(f.rec.Events); n != 0 {
				t.Fatalf("NotRobotIP event repeated, %d new events", n)
			}
		})
	}
}

func TestHCCMManaged(t *testing.T) {
	for providerID, managed := range map[string]bool{
		"":                 false,
		"hcloud://123":     false,
		"hrobot://123":     true,
		"hcloud://bm-123":  true,
		"aws:///eu-1/i-12": false,
	} {
		t.Run(providerID, func(t *testing.T) {
			n := node(nil)
			n.Spec.ProviderID = providerID
			f := newFixture(t, n, pod("smtp", "mail.example.com", time.Now(), true))
			f.r.HCCMManaged = true
			f.reconcile(t)
			if got := f.robot.ptrs[nodeIP] == "mail.example.com"; got != managed {
				t.Fatalf("managed = %v, want %v (reads=%d)", got, managed, f.robot.reads)
			}
			if !managed && f.robot.reads != 0 {
				t.Fatal("robot api called for unmanaged node")
			}
		})
	}
}

func TestForeignIPAfterRead(t *testing.T) {
	// The IP is read while it still belongs to the account and removed from it
	// before the write.
	f := newFixture(t, node(nil))
	f.reconcile(t)
	f.robot.foreign = map[string]bool{nodeIP: true}
	if err := f.r.Create(context.Background(), pod("smtp", "mail.example.com", time.Now(), true)); err != nil {
		t.Fatal(err)
	}

	if res := f.reconcile(t); res.RequeueAfter != 15*time.Minute {
		t.Fatalf("requeueAfter = %v", res.RequeueAfter)
	}
	f.reconcile(t)
	if f.robot.writes != 1 {
		t.Fatalf("writes = %d, want the failed write not to be repeated", f.robot.writes)
	}
}

func TestForeignIPFromAnnotation(t *testing.T) {
	f := newFixture(t, node(map[string]string{DefaultAnnotationPrefix + "/ip": "198.51.100.7"}),
		pod("smtp", "mail.example.com", time.Now(), true))
	f.robot.foreign = map[string]bool{"198.51.100.7": true}
	f.reconcile(t)
	if ev := <-f.rec.Events; !strings.Contains(ev, "from the "+DefaultAnnotationPrefix+"/ip annotation") {
		t.Fatalf("expected event blaming the annotation, got %q", ev)
	}
}

func TestUnmanagedNodeDropsPodLeftAt(t *testing.T) {
	f := newFixture(t, node(map[string]string{DefaultAnnotationPrefix + "/pod-left-at": "2026-10-29T11:59:30Z"}))
	f.robot.ptrs[nodeIP] = "mail.example.com"
	f.r.HCCMManaged = true

	if res := f.reconcile(t); res.RequeueAfter != 0 {
		t.Fatalf("requeueAfter = %v", res.RequeueAfter)
	}
	if got := f.podLeftAt(t); got != "" {
		t.Fatalf("pod-left-at not removed: %q", got)
	}
	if f.robot.reads != 0 || f.robot.ptrs[nodeIP] != "mail.example.com" {
		t.Fatalf("unmanaged node touched, reads=%d ptr=%q", f.robot.reads, f.robot.ptrs[nodeIP])
	}
}
