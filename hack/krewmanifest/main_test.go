package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifest(t *testing.T) {
	dist := t.TempDir()
	for _, p := range platforms {
		os.WriteFile(filepath.Join(dist, "whatchanged_0.2.0_"+p.os+"_"+p.arch+".tar.gz"), []byte(p.os+p.arch), 0o644)
	}
	m, err := Manifest("v0.2.0", "simplyzee/whatchanged", dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"version: v0.2.0",
		"uri: https://github.com/simplyzee/whatchanged/releases/download/v0.2.0/whatchanged_0.2.0_linux_arm64.tar.gz",
		`- from: "whatchanged_0.2.0_windows_amd64/whatchanged.exe"`,
		"bin: whatchanged.exe",
		"sha256: 45b76a1a", // sha256("linuxamd64") prefix
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	if _, err := Manifest("0.2.0", "x/y", dist); err == nil {
		t.Error("version without v should fail")
	}
	if _, err := Manifest("v9.9.9", "x/y", dist); err == nil {
		t.Error("missing archives should fail")
	}
}
