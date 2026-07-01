// Command k8s is a turntable plugin connector that exposes a Kubernetes cluster's
// resources as queryable relations, built on the Go SDK
// (github.com/april/turntable/sdk/go/ttplugin) and client-go.
//
// It is a plugin (not a built-in connector) so client-go — a large dependency
// with strict version pins — stays out of turntable's own dependency graph, the
// same way the procinfo plugin isolates gopsutil.
//
// Datasets: flattened views of the common kinds —
//
//	pods, deployments, statefulsets, daemonsets, nodes, services,
//	namespaces, events
//
// — plus a generic `resource` dataset for any other kind or CRD (returns
// metadata/spec/status as JSON; select the kind with a `resource` option).
//
// Auth is whatever your kubeconfig provides, including AKS/EKS exec credential
// plugins (kubelogin / aws eks get-token) — client-go handles them.
//
// Options (per source):
//
//	kubeconfig  path to a kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
//	context     kubeconfig context to use (default: current-context)
//	namespace   namespace to scope namespaced kinds (default: all namespaces)
//	resource    (generic dataset) the resource name, plural, e.g. "configmaps"
//	group       (generic dataset) API group (default: core "")
//	version     (generic dataset) API version (default: "v1")
//
// Build (its own module; see examples/plugins/build.sh) and register:
//
//	./examples/plugins/build.sh
//	# turntable.yaml:
//	#   k8s: {connector: plugin, command: ["/abs/path/bin/k8s"]}
//	# then: SELECT name, namespace, phase, restarts FROM k8s:pods
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/april/turntable/sdk/go/ttplugin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const listTimeout = 30 * time.Second

func main() {
	if err := ttplugin.Serve(ttplugin.Plugin{
		Name:     "k8s",
		Datasets: datasets(),
	}); err != nil {
		os.Exit(1)
	}
}

func datasets() map[string]ttplugin.Dataset {
	col := func(name, typ string) ttplugin.Column { return ttplugin.Column{Name: name, Type: typ, Nullable: true} }
	return map[string]ttplugin.Dataset{
		"pods": {Schema: schemaOf(
			col("name", "string"), col("namespace", "string"), col("node", "string"),
			col("phase", "string"), col("ready", "string"), col("restarts", "int"),
			col("ip", "string"), col("image", "string"), col("created", "time"),
		), Rows: podRows},
		"deployments": {Schema: workloadSchema(col), Rows: deploymentRows},
		"statefulsets": {Schema: workloadSchema(col), Rows: statefulSetRows},
		"daemonsets": {Schema: schemaOf(
			col("name", "string"), col("namespace", "string"), col("desired", "int"),
			col("ready", "int"), col("available", "int"), col("created", "time"),
		), Rows: daemonSetRows},
		"nodes": {Schema: schemaOf(
			col("name", "string"), col("status", "string"), col("roles", "string"),
			col("version", "string"), col("os_image", "string"), col("cpu", "string"),
			col("memory", "string"), col("unschedulable", "bool"), col("created", "time"),
		), Rows: nodeRows},
		"services": {Schema: schemaOf(
			col("name", "string"), col("namespace", "string"), col("type", "string"),
			col("cluster_ip", "string"), col("external_ip", "string"), col("ports", "string"),
			col("created", "time"),
		), Rows: serviceRows},
		"namespaces": {Schema: schemaOf(
			col("name", "string"), col("status", "string"), col("created", "time"),
		), Rows: namespaceRows},
		"events": {Schema: schemaOf(
			col("namespace", "string"), col("type", "string"), col("reason", "string"),
			col("object", "string"), col("message", "string"), col("count", "int"),
			col("last_seen", "time"),
		), Rows: eventRows},
		"resource": {Schema: schemaOf(
			col("name", "string"), col("namespace", "string"), col("apiVersion", "string"),
			col("kind", "string"), col("created", "time"),
			col("metadata", "any"), col("spec", "any"), col("status", "any"),
		), Rows: resourceRows},
	}
}

func schemaOf(cols ...ttplugin.Column) ttplugin.Schema { return ttplugin.Schema{Columns: cols} }

// workloadSchema is the shared shape for Deployments / StatefulSets.
func workloadSchema(col func(string, string) ttplugin.Column) ttplugin.Schema {
	return schemaOf(
		col("name", "string"), col("namespace", "string"), col("desired", "int"),
		col("ready", "int"), col("updated", "int"), col("available", "int"), col("created", "time"),
	)
}

// ---- row builders ------------------------------------------------------------

func podRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.CoreV1().Pods(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		p := &list.Items[i]
		ready, total, restarts := podStatus(p)
		rows = append(rows, ttplugin.Row{
			p.Name, p.Namespace, p.Spec.NodeName, string(p.Status.Phase),
			fmt.Sprintf("%d/%d", ready, total), restarts, p.Status.PodIP,
			firstImage(p), p.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func deploymentRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.AppsV1().Deployments(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		d := &list.Items[i]
		rows = append(rows, ttplugin.Row{
			d.Name, d.Namespace, int(deref32(d.Spec.Replicas)),
			int(d.Status.ReadyReplicas), int(d.Status.UpdatedReplicas), int(d.Status.AvailableReplicas),
			d.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func statefulSetRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.AppsV1().StatefulSets(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		s := &list.Items[i]
		rows = append(rows, ttplugin.Row{
			s.Name, s.Namespace, int(deref32(s.Spec.Replicas)),
			int(s.Status.ReadyReplicas), int(s.Status.UpdatedReplicas), int(s.Status.AvailableReplicas),
			s.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func daemonSetRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.AppsV1().DaemonSets(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		d := &list.Items[i]
		rows = append(rows, ttplugin.Row{
			d.Name, d.Namespace, int(d.Status.DesiredNumberScheduled),
			int(d.Status.NumberReady), int(d.Status.NumberAvailable), d.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func nodeRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		n := &list.Items[i]
		rows = append(rows, ttplugin.Row{
			n.Name, nodeReady(n), nodeRoles(n), n.Status.NodeInfo.KubeletVersion,
			n.Status.NodeInfo.OSImage, n.Status.Capacity.Cpu().String(),
			n.Status.Capacity.Memory().String(), n.Spec.Unschedulable, n.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func serviceRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.CoreV1().Services(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		s := &list.Items[i]
		rows = append(rows, ttplugin.Row{
			s.Name, s.Namespace, string(s.Spec.Type), s.Spec.ClusterIP,
			externalIP(s), servicePorts(s), s.CreationTimestamp.Time,
		})
	}
	return rows, nil
}

func namespaceRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		n := &list.Items[i]
		rows = append(rows, ttplugin.Row{n.Name, string(n.Status.Phase), n.CreationTimestamp.Time})
	}
	return rows, nil
}

func eventRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cs, _, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	list, err := cs.CoreV1().Events(namespaceOf(req.Options)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		e := &list.Items[i]
		obj := e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name
		rows = append(rows, ttplugin.Row{
			e.Namespace, e.Type, e.Reason, obj, e.Message, int(e.Count), eventLastSeen(e),
		})
	}
	return rows, nil
}

func resourceRows(req ttplugin.Request) (ttplugin.Rows, error) {
	_, dyn, err := clientsFor(req.Options)
	if err != nil {
		return nil, err
	}
	resource := strOpt(req.Options, "resource")
	if resource == "" {
		return nil, fmt.Errorf("k8s `resource` dataset requires a `resource` option (plural name, e.g. configmaps or myresources.example.com)")
	}
	gvr := schema.GroupVersionResource{
		Group:    strOpt(req.Options, "group"),
		Version:  strOrDefault(strOpt(req.Options, "version"), "v1"),
		Resource: resource,
	}
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	ns := namespaceOf(req.Options)
	ri := dyn.Resource(gvr)
	lister := ri.List
	if ns != "" {
		lister = ri.Namespace(ns).List
	}
	list, err := lister(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows ttplugin.Rows
	for i := range list.Items {
		item := &list.Items[i]
		obj := item.Object
		rows = append(rows, ttplugin.Row{
			item.GetName(), item.GetNamespace(), item.GetAPIVersion(), item.GetKind(),
			item.GetCreationTimestamp().Time,
			obj["metadata"], obj["spec"], obj["status"],
		})
	}
	return rows, nil
}

// ---- flatten helpers ---------------------------------------------------------

func podStatus(p *corev1.Pod) (ready, total, restarts int) {
	total = len(p.Status.ContainerStatuses)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += int(cs.RestartCount)
	}
	return ready, total, restarts
}

func firstImage(p *corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Image
	}
	return ""
}

func nodeReady(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k := range n.Labels {
		if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != k && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func externalIP(s *corev1.Service) string {
	var ips []string
	for _, ing := range s.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			ips = append(ips, ing.IP)
		} else if ing.Hostname != "" {
			ips = append(ips, ing.Hostname)
		}
	}
	ips = append(ips, s.Spec.ExternalIPs...)
	return strings.Join(ips, ",")
}

func servicePorts(s *corev1.Service) string {
	var ps []string
	for _, p := range s.Spec.Ports {
		ps = append(ps, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
	}
	return strings.Join(ps, ",")
}

func eventLastSeen(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if e.EventTime.Time != (time.Time{}) {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func deref32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// ---- client construction (cached per kubeconfig+context) --------------------

type clientSet struct {
	typed *kubernetes.Clientset
	dyn   dynamic.Interface
}

var (
	clientsMu    sync.Mutex
	clientsCache = map[string]clientSet{}
)

func clientsFor(opts map[string]any) (*kubernetes.Clientset, dynamic.Interface, error) {
	kubeconfig := strOpt(opts, "kubeconfig")
	kctx := strOpt(opts, "context")
	key := kubeconfig + "\x00" + kctx

	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clientsCache[key]; ok {
		return c.typed, c.dyn, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kctx != "" {
		overrides.CurrentContext = kctx
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	clientsCache[key] = clientSet{typed: typed, dyn: dyn}
	return typed, dyn, nil
}

func namespaceOf(opts map[string]any) string { return strOpt(opts, "namespace") }

func strOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func strOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
