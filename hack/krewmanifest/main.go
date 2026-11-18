// Command krewmanifest writes the krew plugin manifest for a release from
// the archives in dist/. The release workflow runs it and commits the
// result to plugins/whatchanged.yaml, which makes this repository a krew
// custom index:
//
//	kubectl krew index add simplyzee https://github.com/simplyzee/whatchanged.git
//	kubectl krew install simplyzee/whatchanged
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type platform struct{ os, arch string }

var platforms = []platform{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"},
}

func sha(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Manifest renders the krew manifest.
func Manifest(version, repo, dist string) (string, error) {
	if !strings.HasPrefix(version, "v") {
		return "", fmt.Errorf("version must start with v, got %q", version)
	}
	plain := strings.TrimPrefix(version, "v")
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  name: whatchanged
spec:
  version: %s
  homepage: https://github.com/%s
  shortDescription: Show what changed and rank incident culprits
  description: |
    Answers the first question of every incident: what changed?
    Lists recent changes (rollouts, ConfigMaps, Helm releases, Argo CD and
    Flux syncs, RBAC, network policies, nodes, CI deploys…) recorded by the
    whatchanged server and ranks the changes most likely to have caused a
    problem, with the reasons for each score.

      kubectl whatchanged --since 30m -n payments
      kubectl whatchanged suspects -n payments
      kubectl whatchanged show <id>

    Requires the whatchanged server in the cluster (Helm chart:
    https://charts.simplyzee.dev). The plugin reaches it with kubectl
    port-forward automatically.
  caveats: |
    This plugin talks to the whatchanged server. Install it with:
      helm repo add simplyzee https://charts.simplyzee.dev
      helm install whatchanged simplyzee/whatchanged -n whatchanged --create-namespace
  platforms:
`, version, repo)
	for _, p := range platforms {
		name := fmt.Sprintf("whatchanged_%s_%s_%s", plain, p.os, p.arch)
		sum, err := sha(filepath.Join(dist, name+".tar.gz"))
		if err != nil {
			return "", err
		}
		bin := "whatchanged"
		if p.os == "windows" {
			bin += ".exe"
		}
		fmt.Fprintf(&b, `  - selector:
      matchLabels:
        os: %s
        arch: %s
    uri: https://github.com/%s/releases/download/%s/%s.tar.gz
    sha256: %s
    files:
      - from: "%s/%s"
        to: "."
      - from: "%s/LICENSE"
        to: "."
    bin: %s
`, p.os, p.arch, repo, version, name, sum, name, bin, name, bin)
	}
	return b.String(), nil
}

func main() {
	version := flag.String("version", "", "release version, e.g. v0.2.0")
	repo := flag.String("repo", "simplyzee/whatchanged", "GitHub owner/repo")
	dist := flag.String("dist", "dist", "directory with the release archives")
	out := flag.String("out", "plugins/whatchanged.yaml", "output file")
	flag.Parse()
	m, err := Manifest(*version, *repo, *dist)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(m), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
