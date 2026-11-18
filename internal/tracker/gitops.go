package tracker

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

type gitopsApp struct {
	Revision   string
	Ready      string // Healthy/Degraded/… (Argo) or True/False (Flux)
	Message    string
	FinishedAt time.Time
}

func shortRev(r string) string {
	// Flux revisions look like "main@sha1:abc123…", Argo uses bare SHAs,
	// Helm-based ones use chart versions. Keep them readable.
	if i := strings.LastIndex(r, ":"); i >= 0 && len(r)-i > 12 {
		return r[:i+1] + r[i+1:i+9]
	}
	if len(r) == 40 && !strings.ContainsAny(r, "./-@") {
		return r[:8]
	}
	return r
}

// --- Argo CD ---------------------------------------------------------------

func decodeArgo(raw json.RawMessage) (Meta, gitopsApp, error) {
	var obj struct {
		Metadata Meta `json:"metadata"`
		Status   struct {
			Sync struct {
				Revision  string   `json:"revision"`
				Revisions []string `json:"revisions"`
			} `json:"sync"`
			Health struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"health"`
			OperationState struct {
				FinishedAt *time.Time `json:"finishedAt"`
				Phase      string     `json:"phase"`
				SyncResult struct {
					Revision string `json:"revision"`
				} `json:"syncResult"`
			} `json:"operationState"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, gitopsApp{}, err
	}
	s := obj.Status
	a := gitopsApp{Ready: s.Health.Status, Message: s.Health.Message}
	switch {
	case s.OperationState.SyncResult.Revision != "":
		a.Revision = s.OperationState.SyncResult.Revision
	case s.Sync.Revision != "":
		a.Revision = s.Sync.Revision
	default:
		a.Revision = strings.Join(s.Sync.Revisions, ",")
	}
	if s.OperationState.FinishedAt != nil {
		a.FinishedAt = *s.OperationState.FinishedAt
	}
	return obj.Metadata, a, nil
}

func emitSync(e Emitter, m Meta, old, cur gitopsApp) {
	summary := "synced to " + shortRev(cur.Revision)
	if old.Revision != "" {
		summary = fmt.Sprintf("synced %s → %s", shortRev(old.Revision), shortRev(cur.Revision))
	}
	e.Emit(change.ActionSync, change.ImpactHigh, summary,
		At(cur.FinishedAt),
		WithID(m.UID, cur.Revision),
	)
}

func healthSignal(e Emitter, old, cur gitopsApp, bad string) {
	if cur.Ready == bad && old.Ready != bad {
		msg := "became " + bad
		if bad == "False" {
			msg = "not ready"
		}
		if cur.Message != "" {
			msg += ": " + truncate(cur.Message, 200)
		}
		e.Emit(change.ActionWarning, change.ImpactLow, msg)
	}
}

// ArgoApplications tracks Argo CD Applications (optional CRD).
func ArgoApplications(env Env) Tracker {
	return New(Spec[gitopsApp]{
		Name:      "argocd-applications",
		Kind:      "Application",
		Source:    "argocd",
		Optional:  true,
		Resources: []kube.Resource{{Path: "/apis/argoproj.io/v1alpha1/applications"}},
		Decode:    decodeArgo,
		Updated: func(e Emitter, m Meta, old, cur gitopsApp) {
			if cur.Revision != "" && cur.Revision != old.Revision {
				emitSync(e, m, old, cur)
			}
			healthSignal(e, old, cur, "Degraded")
		},
		Backfill: func(e Emitter, m Meta, cur gitopsApp) {
			if cur.Revision != "" && !cur.FinishedAt.IsZero() {
				emitSync(e, m, gitopsApp{}, cur)
			}
		},
	}, env)
}

// --- Flux --------------------------------------------------------------------

func decodeFlux(raw json.RawMessage) (Meta, gitopsApp, error) {
	var obj struct {
		Metadata Meta `json:"metadata"`
		Status   struct {
			LastAppliedRevision string `json:"lastAppliedRevision"`
			History             []struct {
				ChartName    string `json:"chartName"`
				ChartVersion string `json:"chartVersion"`
			} `json:"history"`
			Conditions []struct {
				Type               string     `json:"type"`
				Status             string     `json:"status"`
				Message            string     `json:"message"`
				LastTransitionTime *time.Time `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, gitopsApp{}, err
	}
	a := gitopsApp{Revision: obj.Status.LastAppliedRevision}
	if a.Revision == "" && len(obj.Status.History) > 0 {
		h := obj.Status.History[0]
		a.Revision = h.ChartName + "@" + h.ChartVersion
	}
	for _, c := range obj.Status.Conditions {
		if c.Type == "Ready" {
			a.Ready, a.Message = c.Status, c.Message
		}
	}
	return obj.Metadata, a, nil
}

func fluxSpec(name, kind string, paths ...string) Spec[gitopsApp] {
	res := make([]kube.Resource, len(paths))
	for i, p := range paths {
		res[i] = kube.Resource{Path: p}
	}
	return Spec[gitopsApp]{
		Name:      name,
		Kind:      kind,
		Source:    "flux",
		Optional:  true,
		Resources: res,
		Decode:    decodeFlux,
		Updated: func(e Emitter, m Meta, old, cur gitopsApp) {
			if cur.Revision != "" && cur.Revision != old.Revision {
				emitSync(e, m, old, cur)
			}
			healthSignal(e, old, cur, "False")
		},
	}
}

// FluxKustomizations tracks Flux Kustomizations (optional CRD).
func FluxKustomizations(env Env) Tracker {
	return New(fluxSpec("flux-kustomizations", "Kustomization",
		"/apis/kustomize.toolkit.fluxcd.io/v1/kustomizations",
		"/apis/kustomize.toolkit.fluxcd.io/v1beta2/kustomizations",
	), env)
}

// FluxHelmReleases tracks Flux HelmReleases (optional CRD).
func FluxHelmReleases(env Env) Tracker {
	return New(fluxSpec("flux-helmreleases", "HelmRelease",
		"/apis/helm.toolkit.fluxcd.io/v2/helmreleases",
		"/apis/helm.toolkit.fluxcd.io/v2beta2/helmreleases",
	), env)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
