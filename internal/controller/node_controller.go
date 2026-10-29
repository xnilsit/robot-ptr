// Package controller reconciles the Hetzner Robot reverse DNS entry of each
// node from the annotations of the pods scheduled on it.
package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/xnilsit/robot-ptr/internal/robot"
)

const (
	// DefaultAnnotationPrefix is the default prefix for all annotations read by
	// the controller.
	DefaultAnnotationPrefix = "robot-ptr.nilsit.dev"

	// podNodeNameField indexes pods by spec.nodeName.
	podNodeNameField = "spec.nodeName"
)

// RDNS is the subset of the Robot API used by the controller.
type RDNS interface {
	GetPTR(ctx context.Context, ip string) (string, error)
	SetPTR(ctx context.Context, ip, ptr string) error
	DeletePTR(ctx context.Context, ip string) error
}

// Annotations holds the annotation keys used by the controller.
type Annotations struct {
	// PTR is set on pods and holds the desired PTR of the node they run on.
	PTR string
	// DefaultPTR is set on nodes and overrides the global default PTR.
	DefaultPTR string
	// IP is set on nodes and overrides IP address detection.
	IP string
}

// AnnotationsWithPrefix returns the annotation keys for prefix.
func AnnotationsWithPrefix(prefix string) Annotations {
	return Annotations{
		PTR:        prefix + "/ptr",
		DefaultPTR: prefix + "/default-ptr",
		IP:         prefix + "/ip",
	}
}

// NodeReconciler sets the PTR record of a node's public IP to the annotation
// of the pod running on it, and resets it to a default once no such pod is left.
type NodeReconciler struct {
	client.Client
	Robot       RDNS
	Recorder    events.EventRecorder
	Annotations Annotations

	// NodeSelector restricts the nodes that are managed.
	NodeSelector labels.Selector
	// DefaultPTR is applied when no annotated pod runs on a node. An empty
	// value deletes the entry, which restores Hetzner's generated name.
	DefaultPTR string
	// ResetDelay keeps the last PTR for this long after the annotated pod left
	// the node, so rolling updates do not flap the record.
	ResetDelay time.Duration
	// ResyncInterval is how often the Robot API is re-read to correct drift.
	ResyncInterval time.Duration
	// DryRun logs changes instead of applying them.
	DryRun bool

	// Now is overridable for tests.
	Now func() time.Time

	mu       sync.Mutex
	applied  map[string]appliedPTR // keyed by IP
	lastSeen map[string]time.Time  // keyed by node name

	conflicts map[string]string // last ConflictingPTR message, keyed by node name
}

type appliedPTR struct {
	ptr string
	at  time.Time
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile computes and applies the desired PTR for one node.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			r.clearLastSeen(req.Name)
			r.setConflicts(req.Name, "")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !r.manages(&node) {
		return ctrl.Result{}, nil
	}

	ip, err := r.nodeIP(&node)
	if err != nil {
		log.Info("skipping node without usable public IP", "reason", err.Error())
		return ctrl.Result{}, nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: node.Name}); err != nil {
		return ctrl.Result{}, err
	}

	desired, source, conflicts := r.desiredFromPods(pods.Items)
	r.reportConflicts(&node, desired, source, conflicts)

	now := r.Now()
	if source != "" {
		r.setLastSeen(node.Name, now)
	} else {
		desired = r.defaultPTR(&node)
		if seen, ok := r.getLastSeen(node.Name); ok {
			if wait := seen.Add(r.ResetDelay).Sub(now); wait > 0 {
				log.V(1).Info("delaying reset to default PTR", "remaining", wait)
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
	}
	if err := r.apply(ctx, &node, ip, desired, source); err != nil {
		if robot.IsRateLimited(err) {
			log.Info("robot api rate limit exceeded, backing off")
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		return ctrl.Result{}, err
	}
	if source == "" {
		r.clearLastSeen(node.Name)
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// apply makes sure the PTR of ip is desired. An empty desired value deletes
// the entry. The Robot API is only read once per resync interval.
func (r *NodeReconciler) apply(ctx context.Context, node *corev1.Node, ip, desired, source string) error {
	log := logf.FromContext(ctx).WithValues("ip", ip, "ptr", desired)

	now := r.Now()
	if cached, ok := r.getApplied(ip); ok && cached.ptr == desired && now.Sub(cached.at) < r.ResyncInterval {
		return nil
	}

	current, err := r.Robot.GetPTR(ctx, ip)
	if err != nil {
		return fmt.Errorf("get ptr for %s: %w", ip, err)
	}
	if normalizeCurrent(current) == desired {
		r.setApplied(ip, desired, now)
		return nil
	}

	if r.DryRun {
		log.Info("dry-run: would update PTR", "current", current)
		r.setApplied(ip, desired, now)
		return nil
	}

	if desired == "" {
		err = r.Robot.DeletePTR(ctx, ip)
	} else {
		err = r.Robot.SetPTR(ctx, ip, desired)
	}
	if err != nil {
		return fmt.Errorf("update ptr for %s: %w", ip, err)
	}
	r.setApplied(ip, desired, now)

	reason, msg := "PTRUpdated", fmt.Sprintf("set PTR of %s to %q (was %q)", ip, desired, current)
	if source == "" {
		reason = "PTRReset"
	} else {
		msg += " for pod " + source
	}
	if desired == "" {
		msg = fmt.Sprintf("deleted PTR of %s (was %q)", ip, current)
	}
	log.Info(msg)
	r.Recorder.Eventf(node, nil, corev1.EventTypeNormal, reason, "UpdatePTR", "%s", msg)
	return nil
}

// reportConflicts records a ConflictingPTR event when the conflicting requests
// on node change, instead of on every reconcile.
func (r *NodeReconciler) reportConflicts(node *corev1.Node, desired, source string, conflicts []string) {
	var msg string
	if len(conflicts) > 0 {
		msg = fmt.Sprintf("multiple pods request different PTRs (%s), using %q from %s",
			strings.Join(conflicts, ", "), desired, source)
	}
	if r.setConflicts(node.Name, msg) && msg != "" {
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "ConflictingPTR", "SelectPTR", "%s", msg)
	}
}

// desiredFromPods returns the PTR requested by the oldest eligible pod, the
// pod it came from, and any conflicting requests from other pods.
func (r *NodeReconciler) desiredFromPods(pods []corev1.Pod) (ptr, source string, conflicts []string) {
	var candidates []*corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if ptr := strings.TrimSpace(pod.Annotations[r.Annotations.PTR]); ptr != "" && podActive(pod) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return "", "", nil
	}

	slices.SortFunc(candidates, func(a, b *corev1.Pod) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	ptr = normalizePTR(candidates[0].Annotations[r.Annotations.PTR])
	source = candidates[0].Namespace + "/" + candidates[0].Name
	for _, pod := range candidates[1:] {
		if other := normalizePTR(pod.Annotations[r.Annotations.PTR]); other != ptr {
			conflicts = append(conflicts, fmt.Sprintf("%s/%s=%q", pod.Namespace, pod.Name, other))
		}
	}
	return ptr, source, conflicts
}

func (r *NodeReconciler) defaultPTR(node *corev1.Node) string {
	if v, ok := node.Annotations[r.Annotations.DefaultPTR]; ok {
		return normalizePTR(v)
	}
	return normalizePTR(r.DefaultPTR)
}

func (r *NodeReconciler) manages(node *corev1.Node) bool {
	return r.NodeSelector == nil || r.NodeSelector.Matches(labels.Set(node.Labels))
}

// nodeIP returns the IP whose PTR is managed: the IP annotation if set,
// otherwise the first public IPv4 address of the node.
func (r *NodeReconciler) nodeIP(node *corev1.Node) (string, error) {
	if v := strings.TrimSpace(node.Annotations[r.Annotations.IP]); v != "" {
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return "", fmt.Errorf("invalid %s annotation %q: %w", r.Annotations.IP, v, err)
		}
		return addr.String(), nil
	}
	for _, typ := range []corev1.NodeAddressType{corev1.NodeExternalIP, corev1.NodeInternalIP} {
		for _, a := range node.Status.Addresses {
			if a.Type != typ {
				continue
			}
			addr, err := netip.ParseAddr(a.Address)
			if err == nil && addr.Is4() && isPublic(addr) {
				return addr.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no public IPv4 address and no %s annotation", r.Annotations.IP)
}

func isPublic(addr netip.Addr) bool {
	return addr.IsGlobalUnicast() && !addr.IsPrivate() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(addr)
}

func podActive(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func normalizePTR(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// normalizeCurrent maps Hetzner's generated static.<ip>.clients.your-server.de
// name to "", the value that stands for "no custom entry".
func normalizeCurrent(s string) string {
	s = normalizePTR(s)
	if strings.HasPrefix(s, "static.") && strings.HasSuffix(s, ".clients.your-server.de") {
		return ""
	}
	return s
}

func (r *NodeReconciler) getApplied(ip string) (appliedPTR, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.applied[ip]
	return v, ok
}

func (r *NodeReconciler) setApplied(ip, ptr string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applied == nil {
		r.applied = map[string]appliedPTR{}
	}
	r.applied[ip] = appliedPTR{ptr: ptr, at: at}
}

func (r *NodeReconciler) getLastSeen(node string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.lastSeen[node]
	return v, ok
}

func (r *NodeReconciler) setLastSeen(node string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastSeen == nil {
		r.lastSeen = map[string]time.Time{}
	}
	r.lastSeen[node] = at
}

func (r *NodeReconciler) clearLastSeen(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lastSeen, node)
}

// setConflicts stores the conflict message of node, where "" means no
// conflict, and reports whether it changed.
func (r *NodeReconciler) setConflicts(node, msg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conflicts[node] == msg {
		return false
	}
	if msg == "" {
		delete(r.conflicts, node)
	} else {
		if r.conflicts == nil {
			r.conflicts = map[string]string{}
		}
		r.conflicts[node] = msg
	}
	return true
}

// SetupWithManager registers the controller. Nodes are reconciled directly;
// pod changes enqueue the node the pod is (or was) scheduled on.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podNodeNameField,
		func(obj client.Object) []string {
			if name := obj.(*corev1.Pod).Spec.NodeName; name != "" {
				return []string{name}
			}
			return nil
		}); err != nil {
		return err
	}

	hasPTR := func(obj client.Object) bool {
		_, ok := obj.GetAnnotations()[r.Annotations.PTR]
		return ok
	}
	// Updates also pass when the annotation was just removed from the pod.
	annotated := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return hasPTR(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return hasPTR(e.ObjectOld) || hasPTR(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return hasPTR(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return hasPTR(e.Object) },
	}
	nodeChanged := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		addressesChanged{},
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named("node").
		For(&corev1.Node{}, builder.WithPredicates(nodeChanged)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToNode), builder.WithPredicates(annotated)).
		Complete(r)
}

func podToNode(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pod.Spec.NodeName}}}
}

// addressesChanged passes node updates that change status.addresses, which
// GenerationChangedPredicate ignores because nodes have no status generation.
type addressesChanged struct {
	predicate.Funcs
}

func (addressesChanged) Update(e event.UpdateEvent) bool {
	oldNode, ok1 := e.ObjectOld.(*corev1.Node)
	newNode, ok2 := e.ObjectNew.(*corev1.Node)
	return ok1 && ok2 && !slices.Equal(oldNode.Status.Addresses, newNode.Status.Addresses)
}
