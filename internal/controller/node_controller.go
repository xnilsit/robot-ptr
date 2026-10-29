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
	// PodLeftAt is maintained by the controller on nodes. It records when the
	// last annotated pod left, so the reset delay survives restarts.
	PodLeftAt string
}

// AnnotationsWithPrefix returns the annotation keys for prefix.
func AnnotationsWithPrefix(prefix string) Annotations {
	return Annotations{
		PTR:        prefix + "/ptr",
		DefaultPTR: prefix + "/default-ptr",
		IP:         prefix + "/ip",
		PodLeftAt:  prefix + "/pod-left-at",
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
	// HCCMManaged restricts the managed nodes to those whose spec.providerID
	// hcloud-cloud-controller-manager set to a Hetzner Robot server.
	HCCMManaged bool
	// DefaultPTR is applied when no annotated pod runs on a node. An empty
	// value deletes the entry, which restores Hetzner's generated name.
	DefaultPTR string
	// ResetDelay keeps the last PTR for this long after the annotated pod left
	// the node, so rolling updates do not flap the record. The start of the
	// delay is stored in the PodLeftAt node annotation.
	ResetDelay time.Duration
	// ResyncInterval is how often the Robot API is re-read to correct drift.
	ResyncInterval time.Duration
	// DryRun logs changes instead of applying them.
	DryRun bool

	// Now is overridable for tests.
	Now func() time.Time

	mu        sync.Mutex
	ptrs      map[string]cachedPTR // keyed by IP
	conflicts map[string]string    // last ConflictingPTR message, keyed by node name
}

// cachedPTR is the last known PTR of an IP, normalized like normalizeCurrent,
// or the IP_NOT_FOUND error if the IP does not belong to the Robot account.
type cachedPTR struct {
	ptr string
	err error
	at  time.Time
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile computes and applies the desired PTR for one node.
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
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

	if source != "" {
		if err := r.setPodLeftAt(ctx, &node, nil); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		desired = r.defaultPTR(&node)
		wait, err := r.resetWait(ctx, &node, ip, desired)
		if err != nil {
			return r.robotErrorResult(ctx, &node, ip, err)
		}
		if wait > 0 {
			log.V(1).Info("delaying reset to default PTR", "remaining", wait)
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	if err := r.apply(ctx, &node, ip, desired, source); err != nil {
		return r.robotErrorResult(ctx, &node, ip, err)
	}
	if source == "" {
		if err := r.setPodLeftAt(ctx, &node, nil); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

func (r *NodeReconciler) robotErrorResult(ctx context.Context, node *corev1.Node, ip string, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	switch {
	case robot.IsRateLimited(err):
		log.Info("robot api rate limit exceeded, backing off")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	case robot.IsIPNotFound(err):
		// Retrying cannot help until the IP is added to the account, so wait
		// for the next resync instead of backing off.
		msg := fmt.Sprintf("%s does not belong to the Robot account, set the %s annotation or exclude the node",
			ip, r.Annotations.IP)
		log.Info(msg)
		r.Recorder.Eventf(node, nil, corev1.EventTypeWarning, "NotRobotIP", "UpdatePTR", "%s", msg)
		return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
	}
	return ctrl.Result{}, err
}

// resetWait returns how long the reset of node to its default PTR still has to
// wait. The delay starts when a reconcile first finds the node without an
// annotated pod while its PTR differs from the default; that time is stored in
// the PodLeftAt annotation.
func (r *NodeReconciler) resetWait(ctx context.Context, node *corev1.Node, ip, desired string) (time.Duration, error) {
	if r.ResetDelay <= 0 {
		return 0, nil
	}
	now := r.Now()
	v, ok := node.Annotations[r.Annotations.PodLeftAt]
	if !ok {
		current, err := r.currentPTR(ctx, ip)
		if err != nil || current == desired {
			return 0, err
		}
		if err := r.setPodLeftAt(ctx, node, &now); err != nil {
			return 0, err
		}
		return r.ResetDelay, nil
	}
	leftAt, err := time.Parse(time.RFC3339, v)
	if err != nil {
		logf.FromContext(ctx).Info("ignoring invalid annotation, resetting now",
			"annotation", r.Annotations.PodLeftAt, "value", v)
		return 0, nil
	}
	return max(leftAt.Add(r.ResetDelay).Sub(now), 0), nil
}

// setPodLeftAt sets the PodLeftAt annotation of node to at, or removes it if
// at is nil. Removing a missing annotation does nothing.
func (r *NodeReconciler) setPodLeftAt(ctx context.Context, node *corev1.Node, at *time.Time) error {
	key := r.Annotations.PodLeftAt
	_, exists := node.Annotations[key]
	if at == nil && !exists {
		return nil
	}
	orig := node.DeepCopy()
	if at == nil {
		delete(node.Annotations, key)
	} else {
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[key] = at.UTC().Format(time.RFC3339)
	}
	if err := r.Patch(ctx, node, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("update %s annotation: %w", key, err)
	}
	return nil
}

// currentPTR returns the PTR of ip, read from the Robot API at most once per
// resync interval. Hetzner's generated name is returned as "".
func (r *NodeReconciler) currentPTR(ctx context.Context, ip string) (string, error) {
	if cached, ok := r.getCached(ip); ok && r.Now().Sub(cached.at) < r.ResyncInterval {
		return cached.ptr, cached.err
	}
	current, err := r.Robot.GetPTR(ctx, ip)
	if err != nil {
		err = fmt.Errorf("get ptr for %s: %w", ip, err)
		if robot.IsIPNotFound(err) {
			r.setCached(ip, cachedPTR{err: err})
		}
		return "", err
	}
	current = normalizeCurrent(current)
	r.setCached(ip, cachedPTR{ptr: current})
	return current, nil
}

// apply makes sure the PTR of ip is desired. An empty desired value deletes
// the entry.
func (r *NodeReconciler) apply(ctx context.Context, node *corev1.Node, ip, desired, source string) error {
	log := logf.FromContext(ctx).WithValues("ip", ip, "ptr", desired)

	current, err := r.currentPTR(ctx, ip)
	if err != nil || current == desired {
		return err
	}

	if r.DryRun {
		log.Info("dry-run: would update PTR", "current", current)
		r.setCached(ip, cachedPTR{ptr: desired})
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
	r.setCached(ip, cachedPTR{ptr: desired})

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
	if r.HCCMManaged && !isRobotProviderID(node.Spec.ProviderID) {
		return false
	}
	return r.NodeSelector == nil || r.NodeSelector.Matches(labels.Set(node.Labels))
}

// isRobotProviderID reports whether id is the provider ID
// hcloud-cloud-controller-manager sets on Hetzner Robot servers. Older
// versions used hcloud://bm-<server number>, which nodes registered back then
// keep because a provider ID never changes.
func isRobotProviderID(id string) bool {
	return strings.HasPrefix(id, "hrobot://") || strings.HasPrefix(id, "hcloud://bm-")
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

func (r *NodeReconciler) getCached(ip string) (cachedPTR, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.ptrs[ip]
	return v, ok
}

func (r *NodeReconciler) setCached(ip string, v cachedPTR) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ptrs == nil {
		r.ptrs = map[string]cachedPTR{}
	}
	v.at = r.Now()
	r.ptrs[ip] = v
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
		nodeFieldsChanged{},
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

// nodeFieldsChanged passes node updates that change status.addresses, which
// GenerationChangedPredicate ignores because nodes have no status generation,
// or spec.providerID, which hcloud-cloud-controller-manager sets only after
// the node registered.
type nodeFieldsChanged struct {
	predicate.Funcs
}

func (nodeFieldsChanged) Update(e event.UpdateEvent) bool {
	oldNode, ok1 := e.ObjectOld.(*corev1.Node)
	newNode, ok2 := e.ObjectNew.(*corev1.Node)
	return ok1 && ok2 && (!slices.Equal(oldNode.Status.Addresses, newNode.Status.Addresses) ||
		oldNode.Spec.ProviderID != newNode.Spec.ProviderID)
}
