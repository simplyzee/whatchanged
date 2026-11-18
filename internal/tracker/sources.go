package tracker

import (
	"fmt"
	"sort"
	"strings"

	"github.com/simplyzee/whatchanged/internal/change"
)

// --- Traffic: Ingress, Gateway API, Services --------------------------------------

// Ingresses tracks routing changes per host, TLS and annotations (where
// controllers such as Kong and NGINX take most of their config).
func Ingresses(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "ingresses", Kind: "Ingress",
		Paths: []string{"/apis/networking.k8s.io/v1/ingresses"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			rules, _ := spec["rules"].([]any)
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				host, _ := rm["host"].(string)
				if host == "" {
					host = "*"
				}
				f.put("host "+host, rm["http"])
			}
			f.fields("spec.", spec, "rules")
			f.annotations(mmap(o, "metadata"))
			return f
		},
		Impact:        change.ImpactHigh,
		CreateImpact:  change.ImpactMedium,
		CreateSummary: func(cur fobj) string { return "created (hosts: " + hostsOf(cur) + ")" },
		DeleteImpact:  change.ImpactHigh,
	}, env)
}

func gatewayAPI(name, kind, resource string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Optional: true,
		Paths: []string{
			"/apis/gateway.networking.k8s.io/v1/" + resource,
			"/apis/gateway.networking.k8s.io/v1beta1/" + resource,
		},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			if l, ok := spec["listeners"]; ok {
				f.named("listener", l, "name", nil)
			}
			if r, ok := spec["rules"]; ok {
				f.named("rule", r, "name", nil)
			}
			f.fields("spec.", spec, "listeners", "rules")
			f.annotations(mmap(o, "metadata"))
			return f
		},
		Impact:       change.ImpactHigh,
		CreateImpact: change.ImpactMedium,
		DeleteImpact: change.ImpactHigh,
	}, env)
}

// GatewayAPI tracks Gateways, HTTPRoutes and GRPCRoutes (optional CRDs).
func GatewayAPI(env Env) Tracker {
	return multi{name: "gateway-api", parts: []Tracker{
		gatewayAPI("gateway-api-gateways", "Gateway", "gateways", env),
		gatewayAPI("gateway-api-httproutes", "HTTPRoute", "httproutes", env),
		gatewayAPI("gateway-api-grpcroutes", "GRPCRoute", "grpcroutes", env),
	}}
}

// Services tracks selector, port and type changes (and annotations, which
// drive cloud load balancers).
func Services(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "services", Kind: "Service",
		Paths: []string{"/api/v1/services"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			f.named("port", spec["ports"], "name", nil)
			f.fields("spec.", spec, "ports", "clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort")
			f.annotations(mmap(o, "metadata"))
			return f
		},
		ImpactFor: func(_, _ fobj, changed, added, removed []string) change.Impact {
			all := append(append(append([]string{}, changed...), added...), removed...)
			if anyMatch(all, func(k string) bool {
				return k == "spec.selector" || k == "spec.type" || strings.HasPrefix(k, "port ")
			}) {
				return change.ImpactHigh
			}
			return change.ImpactMedium
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactHigh,
	}, env)
}

// --- Scaling: HPA, KEDA, PDB ----------------------------------------------------

// HPAs tracks HorizontalPodAutoscaler bounds and metrics, and records a
// signal when an HPA hits its max replicas.
func HPAs(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "hpas", Kind: "HorizontalPodAutoscaler",
		Paths: []string{"/apis/autoscaling/v2/horizontalpodautoscalers"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			f.fields("spec.", spec)
			cur, _ := mget(o, "status", "currentReplicas").(float64)
			max, _ := spec["maxReplicas"].(float64)
			f.State["atMax"] = fmt.Sprint(max > 0 && cur >= max)
			f.State["max"] = fmt.Sprint(max)
			return f
		},
		Signals: func(e Emitter, old, cur fobj) {
			if cur.State["atMax"] == "true" && old.State["atMax"] != "true" {
				e.Emit(change.ActionWarning, change.ImpactLow, "MaxReplicas: scaled to maxReplicas ("+cur.State["max"]+") and cannot scale further")
			}
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

// KEDAScaledObjects tracks KEDA ScaledObjects (optional CRD).
func KEDAScaledObjects(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "keda-scaledobjects", Kind: "ScaledObject", Source: "keda", Optional: true,
		Paths: []string{"/apis/keda.sh/v1alpha1/scaledobjects"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			f.named("trigger", spec["triggers"], "name", nil)
			f.fields("spec.", spec, "triggers")
			return f
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

// PDBs tracks PodDisruptionBudgets.
func PDBs(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "pdbs", Kind: "PodDisruptionBudget",
		Paths: []string{"/apis/policy/v1/poddisruptionbudgets"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.fields("spec.", mmap(o, "spec"))
			return f
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

// --- Scheduling and quotas ------------------------------------------------------

// PriorityClasses tracks PriorityClasses.
func PriorityClasses(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "priorityclasses", Kind: "PriorityClass",
		Paths: []string{"/apis/scheduling.k8s.io/v1/priorityclasses"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			for _, k := range []string{"value", "globalDefault", "preemptionPolicy", "description"} {
				if v, ok := o[k]; ok {
					f.put(k, v)
				}
			}
			return f
		},
		CreateImpact: change.ImpactMedium,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

// ResourceQuotas tracks quota limits per resource.
func ResourceQuotas(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "resourcequotas", Kind: "ResourceQuota",
		Paths: []string{"/api/v1/resourcequotas"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			for k, v := range mmap(spec, "hard") {
				f.put("hard "+k, v)
			}
			f.fields("spec.", spec, "hard")
			return f
		},
		CreateImpact: change.ImpactMedium,
		DeleteImpact: change.ImpactLow,
	}, env)
}

// LimitRanges tracks default and max container limits.
func LimitRanges(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "limitranges", Kind: "LimitRange",
		Paths: []string{"/api/v1/limitranges"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.named("limits", mget(o, "spec", "limits"), "type", nil)
			return f
		},
		CreateImpact: change.ImpactMedium,
		DeleteImpact: change.ImpactLow,
	}, env)
}

// --- API extensions and admission ---------------------------------------------------

// CRDs tracks CustomResourceDefinitions: versions served/stored and schemas.
func CRDs(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "crds", Kind: "CustomResourceDefinition",
		Paths: []string{"/apis/apiextensions.k8s.io/v1/customresourcedefinitions"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			spec := mmap(o, "spec")
			versions, _ := spec["versions"].([]any)
			for _, v := range versions {
				vm, _ := v.(map[string]any)
				n, _ := vm["name"].(string)
				f.put("version "+n+" served", vm["served"])
				f.put("version "+n+" storage", vm["storage"])
				f.put("version "+n+" schema", vm["schema"])
				rest := cloneMap(vm)
				for _, k := range []string{"name", "served", "storage", "schema"} {
					delete(rest, k)
				}
				f.put("version "+n+" settings", rest)
			}
			f.fields("spec.", spec, "versions")
			return f
		},
		ImpactFor: func(old, cur fobj, changed, _, removed []string) change.Impact {
			if anyMatch(removed, func(k string) bool { return strings.HasPrefix(k, "version ") }) {
				return change.ImpactHigh
			}
			for _, k := range changed {
				if strings.HasSuffix(k, " served") && cur.Show[k] == "false" {
					return change.ImpactHigh
				}
			}
			if anyMatch(changed, func(k string) bool { return k == "spec.conversion" }) {
				return change.ImpactHigh
			}
			return change.ImpactMedium
		},
		CreateImpact: change.ImpactMedium,
		CreateSummary: func(cur fobj) string {
			var vs []string
			for k := range cur.Keys {
				if n, ok := strings.CutSuffix(strings.TrimPrefix(k, "version "), " served"); ok && strings.HasPrefix(k, "version ") {
					vs = append(vs, n)
				}
			}
			sort.Strings(vs)
			return "CRD installed (" + strings.Join(vs, ", ") + ")"
		},
		DeleteImpact: change.ImpactHigh,
	}, env)
}

func webhookConfig(name, kind, resource string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind,
		Paths: []string{"/apis/admissionregistration.k8s.io/v1/" + resource},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			hooks, _ := o["webhooks"].([]any)
			for i, h := range hooks {
				hm, _ := h.(map[string]any)
				n, _ := hm["name"].(string)
				if n == "" {
					n = fmt.Sprint(i)
				}
				rest := cloneMap(hm)
				// caBundle is rotated by cert-manager & co: pure noise.
				if cc, ok := rest["clientConfig"].(map[string]any); ok {
					cc = cloneMap(cc)
					delete(cc, "caBundle")
					rest["clientConfig"] = cc
				}
				f.put("webhook "+n+" failurePolicy", hm["failurePolicy"])
				delete(rest, "failurePolicy")
				f.put("webhook "+n, rest)
			}
			return f
		},
		ImpactFor: func(_, _ fobj, _, _, _ []string) change.Impact { return change.ImpactHigh },
		Describe: func(old, cur fobj, s string) string {
			for k, v := range cur.Show {
				if strings.HasSuffix(k, " failurePolicy") && old.Show[k] != "" && old.Show[k] != v && v == "Fail" {
					return s + " (now fails closed: requests are rejected if the webhook is down)"
				}
			}
			return s
		},
		CreateImpact: change.ImpactHigh,
		CreateSummary: func(cur fobj) string {
			n := 0
			for k := range cur.Keys {
				if strings.HasPrefix(k, "webhook ") && !strings.HasSuffix(k, " failurePolicy") {
					n++
				}
			}
			return fmt.Sprintf("created (%d webhook(s))", n)
		},
		DeleteImpact: change.ImpactHigh,
	}, env)
}

func admissionPolicy(name, kind, resource string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Optional: true,
		Paths: []string{
			"/apis/admissionregistration.k8s.io/v1/" + resource,
			"/apis/admissionregistration.k8s.io/v1beta1/" + resource,
		},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.fields("spec.", mmap(o, "spec"))
			return f
		},
		Impact:       change.ImpactHigh,
		CreateImpact: change.ImpactHigh,
		DeleteImpact: change.ImpactHigh,
	}, env)
}

// Admission tracks admission webhooks and ValidatingAdmissionPolicies: a
// broken webhook can block every deploy in the cluster.
func Admission(env Env) Tracker {
	return multi{name: "admission", parts: []Tracker{
		webhookConfig("admission-validatingwebhooks", "ValidatingWebhookConfiguration", "validatingwebhookconfigurations", env),
		webhookConfig("admission-mutatingwebhooks", "MutatingWebhookConfiguration", "mutatingwebhookconfigurations", env),
		admissionPolicy("admission-policies", "ValidatingAdmissionPolicy", "validatingadmissionpolicies", env),
		admissionPolicy("admission-policybindings", "ValidatingAdmissionPolicyBinding", "validatingadmissionpolicybindings", env),
	}}
}

// --- RBAC ---------------------------------------------------------------------

func listStr(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func ruleKey(rm map[string]any) string {
	res := listStr(rm["resources"])
	groups := listStr(rm["apiGroups"])
	for i, r := range res {
		if len(groups) == 1 && groups[0] != "" {
			res[i] = r + "." + groups[0]
		}
	}
	target := strings.Join(res, ",")
	if urls := listStr(rm["nonResourceURLs"]); len(urls) > 0 {
		target = strings.Join(urls, ",")
	}
	key := "rule " + strings.Join(listStr(rm["verbs"]), ",") + " " + target
	if names := listStr(rm["resourceNames"]); len(names) > 0 {
		key += " (" + strings.Join(names, ",") + ")"
	}
	return key
}

func roleSpec(name, kind, path string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Source: "rbac",
		Paths: []string{path},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			rules, _ := o["rules"].([]any)
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				f.put(ruleKey(rm), rm)
			}
			if ag, ok := o["aggregationRule"]; ok {
				f.put("aggregationRule", ag)
				f.State["aggregated"] = "true"
			}
			return f
		},
		ImpactFor: func(_, cur fobj, changed, added, _ []string) change.Impact {
			if cur.State["aggregated"] == "true" {
				return change.ImpactLow // rewritten by the aggregation controller
			}
			if anyMatch(append(changed, added...), func(k string) bool {
				return strings.Contains(k, "*") || strings.Contains(k, "secrets") || strings.Contains(k, "escalate") || strings.Contains(k, "impersonate")
			}) {
				return change.ImpactHigh
			}
			return change.ImpactMedium
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

func bindingSpec(name, kind, path string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Source: "rbac",
		Paths: []string{path},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			rr := mmap(o, "roleRef")
			f.put("roleRef", mstr(rr, "kind")+"/"+mstr(rr, "name"))
			subs, _ := o["subjects"].([]any)
			for _, s := range subs {
				sm, _ := s.(map[string]any)
				id := mstr(sm, "kind") + ":" + mstr(sm, "name")
				if ns := mstr(sm, "namespace"); ns != "" {
					id = mstr(sm, "kind") + ":" + ns + "/" + mstr(sm, "name")
				}
				f.put("subject "+id, true)
				delete(f.Show, "subject "+id)
			}
			f.State["role"] = mstr(rr, "name")
			return f
		},
		ImpactFor: func(_, cur fobj, _, _, _ []string) change.Impact {
			if cur.State["role"] == "cluster-admin" || cur.State["role"] == "admin" {
				return change.ImpactHigh
			}
			return change.ImpactMedium
		},
		Describe: func(_, cur fobj, s string) string {
			s = strings.ReplaceAll(s, "added subject ", "granted to ")
			s = strings.ReplaceAll(s, "removed subject ", "revoked from ")
			return s + " (" + cur.Show["roleRef"] + ")"
		},
		CreateImpact:  change.ImpactMedium,
		CreateSummary: func(cur fobj) string { return "binds " + cur.Show["roleRef"] + " to " + subjectsOf(cur) },
		DeleteImpact:  change.ImpactMedium,
	}, env)
}

func subjectsOf(f fobj) string {
	var s []string
	for k := range f.Keys {
		if id, ok := strings.CutPrefix(k, "subject "); ok {
			s = append(s, id)
		}
	}
	sort.Strings(s)
	return limitList(s, 3)
}

// RBAC tracks Roles, ClusterRoles and their bindings.
func RBAC(env Env) Tracker {
	return multi{name: "rbac", parts: []Tracker{
		roleSpec("rbac-clusterroles", "ClusterRole", "/apis/rbac.authorization.k8s.io/v1/clusterroles", env),
		roleSpec("rbac-roles", "Role", "/apis/rbac.authorization.k8s.io/v1/roles", env),
		bindingSpec("rbac-clusterrolebindings", "ClusterRoleBinding", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", env),
		bindingSpec("rbac-rolebindings", "RoleBinding", "/apis/rbac.authorization.k8s.io/v1/rolebindings", env),
	}}
}

// --- cert-manager ---------------------------------------------------------------

func certificates(env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: "cert-manager-certificates", Kind: "Certificate", Source: "cert-manager", Optional: true,
		Paths: []string{"/apis/cert-manager.io/v1/certificates"},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.fields("spec.", mmap(o, "spec"))
			rev, _ := mget(o, "status", "revision").(float64)
			f.State["revision"] = fmt.Sprint(int(rev))
			f.State["ready"], f.State["message"] = condition(o, "Ready")
			return f
		},
		Signals: func(e Emitter, old, cur fobj) {
			if cur.State["revision"] != old.State["revision"] && old.State["revision"] != "0" && cur.State["revision"] != "0" {
				e.Emit("renew", change.ImpactLow, "certificate renewed (revision "+cur.State["revision"]+")")
			}
			if cur.State["ready"] == "False" && old.State["ready"] != "False" {
				e.Emit(change.ActionWarning, change.ImpactLow, "CertificateNotReady: "+truncate(cur.State["message"], 200))
			}
		},
		CreateImpact: change.ImpactLow,
		DeleteImpact: change.ImpactMedium,
	}, env)
}

func issuers(name, kind, resource string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Source: "cert-manager", Optional: true,
		Paths: []string{"/apis/cert-manager.io/v1/" + resource},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.fields("spec.", mmap(o, "spec"))
			f.State["ready"], f.State["message"] = condition(o, "Ready")
			return f
		},
		Impact: change.ImpactHigh,
		Signals: func(e Emitter, old, cur fobj) {
			if cur.State["ready"] == "False" && old.State["ready"] != "False" {
				e.Emit(change.ActionWarning, change.ImpactLow, "IssuerNotReady: "+truncate(cur.State["message"], 200))
			}
		},
		CreateImpact: change.ImpactMedium,
		DeleteImpact: change.ImpactHigh,
	}, env)
}

// CertManager tracks Certificates (spec changes, renewals, readiness) and
// Issuers/ClusterIssuers (optional CRDs).
func CertManager(env Env) Tracker {
	return multi{name: "cert-manager", parts: []Tracker{
		certificates(env),
		issuers("cert-manager-issuers", "Issuer", "issuers", env),
		issuers("cert-manager-clusterissuers", "ClusterIssuer", "clusterissuers", env),
	}}
}

// --- Kong -------------------------------------------------------------------------

func kongPlugin(name, kind, resource string, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Source: "kong", Optional: true,
		Paths: []string{"/apis/configuration.konghq.com/v1/" + resource},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			for _, k := range []string{"plugin", "config", "configFrom", "configPatches", "disabled", "protocols", "ordering", "consumerRef", "instance_name"} {
				if v, ok := o[k]; ok {
					f.put(k, v)
				}
			}
			cfg, _ := o["config"].(map[string]any)
			for k, v := range cfg {
				f.put("config."+k, v)
			}
			delete(f.Keys, "config")
			return f
		},
		Impact: change.ImpactHigh,
		Describe: func(_, cur fobj, s string) string {
			if p := cur.Show["plugin"]; p != "" {
				return p + " plugin: " + s
			}
			return s
		},
		CreateImpact:  change.ImpactHigh,
		CreateSummary: func(cur fobj) string { return cur.Show["plugin"] + " plugin created" },
		DeleteImpact:  change.ImpactHigh,
	}, env)
}

// Kong tracks KongPlugins, KongClusterPlugins and KongIngresses (optional
// CRDs). Plugin attachments via konghq.com/plugins annotations are picked
// up by the Ingress and Service trackers.
func Kong(env Env) Tracker {
	return multi{name: "kong", parts: []Tracker{
		kongPlugin("kong-plugins", "KongPlugin", "kongplugins", env),
		kongPlugin("kong-clusterplugins", "KongClusterPlugin", "kongclusterplugins", env),
		fieldTracker(FieldSpec{
			Name: "kong-ingresses", Kind: "KongIngress", Source: "kong", Optional: true,
			Paths: []string{"/apis/configuration.konghq.com/v1/kongingresses"},
			Fingerprint: func(o map[string]any) fobj {
				f := newFobj()
				for _, k := range []string{"proxy", "route", "upstream"} {
					for sk, v := range mmap(o, k) {
						f.put(k+"."+sk, v)
					}
				}
				return f
			},
			Impact:       change.ImpactHigh,
			CreateImpact: change.ImpactMedium,
			DeleteImpact: change.ImpactHigh,
		}, env),
	}}
}

// --- Cilium networking --------------------------------------------------------------

func ciliumSpec(name, kind, resource string, impact change.Impact, env Env) Tracker {
	return fieldTracker(FieldSpec{
		Name: name, Kind: kind, Source: "cilium", Optional: true,
		Paths: []string{"/apis/cilium.io/v2/" + resource, "/apis/cilium.io/v2alpha1/" + resource},
		Fingerprint: func(o map[string]any) fobj {
			f := newFobj()
			f.fields("spec.", mmap(o, "spec"))
			return f
		},
		Impact:       impact,
		CreateImpact: impact,
		DeleteImpact: change.ImpactHigh,
	}, env)
}

// CiliumNetworking tracks Cilium egress gateway, BGP, LB IPAM and L2
// announcement config (optional CRDs).
func CiliumNetworking(env Env) Tracker {
	return multi{name: "cilium-networking", parts: []Tracker{
		ciliumSpec("cilium-egressgatewaypolicies", "CiliumEgressGatewayPolicy", "ciliumegressgatewaypolicies", change.ImpactHigh, env),
		ciliumSpec("cilium-bgppeeringpolicies", "CiliumBGPPeeringPolicy", "ciliumbgppeeringpolicies", change.ImpactHigh, env),
		ciliumSpec("cilium-bgpclusterconfigs", "CiliumBGPClusterConfig", "ciliumbgpclusterconfigs", change.ImpactHigh, env),
		ciliumSpec("cilium-bgppeerconfigs", "CiliumBGPPeerConfig", "ciliumbgppeerconfigs", change.ImpactHigh, env),
		ciliumSpec("cilium-bgpadvertisements", "CiliumBGPAdvertisement", "ciliumbgpadvertisements", change.ImpactHigh, env),
		ciliumSpec("cilium-loadbalancerippools", "CiliumLoadBalancerIPPool", "ciliumloadbalancerippools", change.ImpactMedium, env),
		ciliumSpec("cilium-l2announcementpolicies", "CiliumL2AnnouncementPolicy", "ciliuml2announcementpolicies", change.ImpactMedium, env),
	}}
}
