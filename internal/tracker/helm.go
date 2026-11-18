package tracker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// helmRelease is read from the labels on Helm's release Secrets
// (sh.helm.release.v1.<name>.v<revision>). We only ever fetch metadata, so
// the (large, sensitive) release payload never leaves the API server.
type helmRelease struct {
	Release  string
	Revision int
	Status   string
	Created  time.Time
}

func decodeHelm(raw json.RawMessage) (Meta, helmRelease, error) {
	var obj struct {
		Metadata Meta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Meta{}, helmRelease{}, err
	}
	l := obj.Metadata.Labels
	rev, _ := strconv.Atoi(l["version"])
	return obj.Metadata, helmRelease{
		Release:  l["name"],
		Revision: rev,
		Status:   l["status"],
		Created:  obj.Metadata.CreationTimestamp,
	}, nil
}

func helmVerb(r helmRelease, rollback bool) string {
	switch {
	case rollback:
		return "rolled back"
	case r.Revision <= 1:
		return "installed"
	default:
		return "upgraded"
	}
}

func emitHelm(e Emitter, m Meta, r helmRelease, rollback bool, includeStatus bool) {
	if r.Release == "" {
		return
	}
	impact := change.ImpactHigh
	summary := fmt.Sprintf("%s to revision %d", helmVerb(r, rollback), r.Revision)
	if includeStatus && r.Status != "" {
		summary += " (" + r.Status + ")"
	}
	e.Emit(change.ActionRelease, impact, summary,
		Object("HelmRelease", m.Namespace, r.Release),
		At(r.Created),
		Actor("helm"),
		// One record per release revision, whether seen live or backfilled.
		WithID(m.Namespace, r.Release, strconv.Itoa(r.Revision)),
	)
}

func pending(status string) bool { return strings.HasPrefix(status, "pending") }

// Helm tracks Helm 3 releases via their storage Secrets.
func Helm(env Env) Tracker {
	return New(Spec[helmRelease]{
		Name:   "helm-releases",
		Kind:   "HelmRelease",
		Source: "helm",
		Resources: []kube.Resource{{
			Path:          "/api/v1/secrets",
			LabelSelector: "owner=helm",
			MetadataOnly:  true,
		}},
		Decode: decodeHelm,
		Added: func(e Emitter, m Meta, r helmRelease) {
			// Wait for the outcome unless the release is already settled.
			if !pending(r.Status) {
				emitHelm(e, m, r, false, true)
			}
		},
		Updated: func(e Emitter, m Meta, old, cur helmRelease) {
			if pending(old.Status) && !pending(cur.Status) {
				emitHelm(e, m, cur, old.Status == "pending-rollback", true)
			}
		},
		Backfill: func(e Emitter, m Meta, r helmRelease) {
			if !pending(r.Status) {
				// Superseded just means "a later revision exists": not useful.
				emitHelm(e, m, r, false, r.Status != "superseded")
			}
		},
	}, env)
}
