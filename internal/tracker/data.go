package tracker

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/simplyzee/whatchanged/internal/change"
	"github.com/simplyzee/whatchanged/internal/kube"
)

// dataObj fingerprints ConfigMaps and Secrets: key name -> hash of value.
// Values are never stored, only hashed.
type dataObj struct {
	Keys map[string]string
	Skip bool
}

func decodeData(skip func(name, typ string) bool) func(json.RawMessage) (Meta, dataObj, error) {
	return func(raw json.RawMessage) (Meta, dataObj, error) {
		var obj struct {
			Metadata   Meta                       `json:"metadata"`
			Type       string                     `json:"type"`
			Data       map[string]json.RawMessage `json:"data"`
			BinaryData map[string]json.RawMessage `json:"binaryData"`
			StringData map[string]json.RawMessage `json:"stringData"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return Meta{}, dataObj{}, err
		}
		d := dataObj{Keys: map[string]string{}, Skip: skip(obj.Metadata.Name, obj.Type)}
		for _, m := range []map[string]json.RawMessage{obj.Data, obj.BinaryData, obj.StringData} {
			for k, v := range m {
				d.Keys[k] = hashBytes(v)
			}
		}
		return obj.Metadata, d, nil
	}
}

func diffData(e Emitter, noun string, old, cur dataObj) {
	if cur.Skip {
		return
	}
	changed, added, removed := diffKeys(old.Keys, cur.Keys)
	var parts []string
	if len(changed) > 0 {
		parts = append(parts, "changed "+limitList(changed, 5))
	}
	if len(added) > 0 {
		parts = append(parts, "added "+limitList(added, 5))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+limitList(removed, 5))
	}
	if len(parts) == 0 {
		return // only labels/annotations moved
	}
	e.Emit(change.ActionConfig, change.ImpactMedium, noun+" keys "+strings.Join(parts, "; "))
}

func dataSpec(name, kind, noun string, res kube.Resource, skip func(name, typ string) bool) Spec[dataObj] {
	return Spec[dataObj]{
		Name:      name,
		Kind:      kind,
		Source:    "kubernetes",
		Resources: []kube.Resource{res},
		Decode:    decodeData(skip),
		Added: func(e Emitter, m Meta, v dataObj) {
			if !v.Skip {
				e.Emit(change.ActionCreate, change.ImpactLow, "created", At(m.CreationTimestamp))
			}
		},
		Updated: func(e Emitter, _ Meta, old, cur dataObj) { diffData(e, noun, old, cur) },
		Deleted: func(e Emitter, _ Meta, v dataObj) {
			if !v.Skip {
				e.Emit(change.ActionDelete, change.ImpactMedium, "deleted")
			}
		},
	}
}

// ConfigMaps tracks data changes in ConfigMaps.
func ConfigMaps(sink Sink, now func() time.Time) Tracker {
	skip := func(name, _ string) bool { return name == "kube-root-ca.crt" }
	return New(dataSpec("configmaps", "ConfigMap", "data", kube.Resource{Path: "/api/v1/configmaps"}, skip), sink, now)
}

// Secrets tracks which keys changed in Secrets. Values are hashed in memory
// and never stored or logged. Helm release and service-account token
// secrets are ignored.
func Secrets(sink Sink, now func() time.Time) Tracker {
	skip := func(_, typ string) bool {
		return typ == "helm.sh/release.v1" || typ == "kubernetes.io/service-account-token"
	}
	res := kube.Resource{Path: "/api/v1/secrets", FieldSelector: "type!=helm.sh/release.v1"}
	return New(dataSpec("secrets", "Secret", "secret", res, skip), sink, now)
}
