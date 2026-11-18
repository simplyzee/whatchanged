package tracker

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// --- Nodes -------------------------------------------------------------------

type node struct {
	Unschedulable bool
	Ready         string
	Kubelet       string
}

func decodeNode(raw json.RawMessage) (Meta, node, error) {
	var obj struct {
		Metadata Meta `json:"metadata"`
		Spec     struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			NodeInfo struct {
				KubeletVersion string `json:"kubeletVersion"`
			} `json:"nodeInfo"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, node{}, err
	}
	n := node{Unschedulable: obj.Spec.Unschedulable, Kubelet: obj.Status.NodeInfo.KubeletVersion}
	for _, c := range obj.Status.Conditions {
		if c.Type == "Ready" {
			n.Ready = c.Status
		}
	}
	return obj.Metadata, n, nil
}

// Nodes tracks node joins/removals, cordons, readiness and kubelet upgrades.
func Nodes(env Env) Tracker {
	return New(Spec[node]{
		Name:      "nodes",
		Kind:      "Node",
		Source:    "kubernetes",
		Resources: []kube.Resource{{Path: "/api/v1/nodes"}},
		Decode:    decodeNode,
		Added: func(e Emitter, m Meta, n node) {
			e.Emit(change.ActionNode, change.ImpactMedium, "node joined ("+n.Kubelet+")", At(m.CreationTimestamp))
		},
		Updated: func(e Emitter, _ Meta, old, cur node) {
			if old.Unschedulable != cur.Unschedulable {
				if cur.Unschedulable {
					e.Emit(change.ActionCordon, change.ImpactMedium, "cordoned")
				} else {
					e.Emit(change.ActionCordon, change.ImpactLow, "uncordoned")
				}
			}
			if old.Ready != cur.Ready && old.Ready != "" {
				if cur.Ready == "True" {
					e.Emit(change.ActionNode, change.ImpactLow, "became Ready")
				} else {
					e.Emit(change.ActionNode, change.ImpactHigh, "became NotReady (Ready="+cur.Ready+")")
				}
			}
			if old.Kubelet != cur.Kubelet && old.Kubelet != "" {
				e.Emit(change.ActionNode, change.ImpactMedium, fmt.Sprintf("kubelet %s → %s", old.Kubelet, cur.Kubelet))
			}
		},
		Deleted: func(e Emitter, _ Meta, _ node) {
			e.Emit(change.ActionNode, change.ImpactMedium, "node removed")
		},
	}, env)
}

// --- Network policies ----------------------------------------------------------

type policy struct{ Hash string }

func decodePolicy(raw json.RawMessage) (Meta, policy, error) {
	var obj struct {
		Metadata Meta            `json:"metadata"`
		Spec     json.RawMessage `json:"spec"`
		Specs    json.RawMessage `json:"specs"` // Cilium allows a list
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, policy{}, err
	}
	return obj.Metadata, policy{Hash: hashBytes(append(obj.Spec, obj.Specs...))}, nil
}

func policySpec(name, kind, source string, optional bool, path string) Spec[policy] {
	return Spec[policy]{
		Name:      name,
		Kind:      kind,
		Source:    source,
		Optional:  optional,
		Resources: []kube.Resource{{Path: path}},
		Decode:    decodePolicy,
		Added: func(e Emitter, m Meta, _ policy) {
			e.Emit(change.ActionPolicy, change.ImpactHigh, "policy created", At(m.CreationTimestamp))
		},
		Updated: func(e Emitter, _ Meta, old, cur policy) {
			if old.Hash != cur.Hash {
				e.Emit(change.ActionPolicy, change.ImpactHigh, "policy rules changed")
			}
		},
		Deleted: func(e Emitter, _ Meta, _ policy) {
			e.Emit(change.ActionPolicy, change.ImpactHigh, "policy deleted")
		},
	}
}

// NetworkPolicies tracks networking.k8s.io NetworkPolicies.
func NetworkPolicies(env Env) Tracker {
	return New(policySpec("networkpolicies", "NetworkPolicy", "kubernetes", false,
		"/apis/networking.k8s.io/v1/networkpolicies"), env)
}

// CiliumNetworkPolicies tracks CiliumNetworkPolicies (optional CRD).
func CiliumNetworkPolicies(env Env) Tracker {
	return New(policySpec("cilium-networkpolicies", "CiliumNetworkPolicy", "cilium", true,
		"/apis/cilium.io/v2/ciliumnetworkpolicies"), env)
}

// CiliumClusterwideNetworkPolicies tracks CCNPs (optional CRD).
func CiliumClusterwideNetworkPolicies(env Env) Tracker {
	return New(policySpec("cilium-clusterwidenetworkpolicies", "CiliumClusterwideNetworkPolicy", "cilium", true,
		"/apis/cilium.io/v2/ciliumclusterwidenetworkpolicies"), env)
}

// --- Warning events ------------------------------------------------------------

type warning struct {
	Kind, Namespace, Name, Reason, Message string
	Time                                   time.Time
}

func decodeEvent(raw json.RawMessage) (Meta, warning, error) {
	var obj struct {
		Metadata       Meta `json:"metadata"`
		InvolvedObject struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"involvedObject"`
		Reason         string     `json:"reason"`
		Message        string     `json:"message"`
		LastTimestamp  *time.Time `json:"lastTimestamp"`
		FirstTimestamp *time.Time `json:"firstTimestamp"`
		EventTime      *time.Time `json:"eventTime"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, warning{}, err
	}
	w := warning{
		Kind:      obj.InvolvedObject.Kind,
		Namespace: obj.InvolvedObject.Namespace,
		Name:      obj.InvolvedObject.Name,
		Reason:    obj.Reason,
		Message:   obj.Message,
		Time:      obj.Metadata.CreationTimestamp,
	}
	for _, t := range []*time.Time{obj.LastTimestamp, obj.EventTime, obj.FirstTimestamp} {
		if t != nil && !t.IsZero() {
			w.Time = *t
			break
		}
	}
	if w.Namespace == "" {
		w.Namespace = obj.Metadata.Namespace
	}
	return obj.Metadata, w, nil
}

func emitWarning(e Emitter, m Meta, w warning) {
	opts := []Opt{Object(w.Kind, w.Namespace, w.Name), At(w.Time), Actor(""), WithID(m.UID)}
	owner := e.Graph().Owner(w.Namespace, w.Kind, w.Name)
	if owner == "" && len(e.Graph().Related(w.Namespace, w.Kind, w.Name)) > 0 {
		owner = w.Kind + "/" + w.Name // the event is about a workload itself
	}
	if owner != "" {
		opts = append(opts, Label("owner", owner))
	}
	e.Emit(change.ActionWarning, change.ImpactLow, w.Reason+": "+truncate(w.Message, 240), opts...)
}

// WarningEvents records Warning events as signals. They are never ranked as
// suspects, but they show symptoms on the same timeline as the changes.
func WarningEvents(env Env) Tracker {
	return New(Spec[warning]{
		Name:      "warning-events",
		Kind:      "Event",
		Source:    "event",
		Resources: []kube.Resource{{Path: "/api/v1/events", FieldSelector: "type=Warning"}},
		Decode:    decodeEvent,
		Added:     emitWarning,
		Backfill:  emitWarning,
	}, env)
}
