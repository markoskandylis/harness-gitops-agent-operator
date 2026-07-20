package bootstrap

import (
	"os"
	"strings"
	"testing"
)

func TestAppProjectRunsAfterBundledArgoCRDs(t *testing.T) {
	manifest, err := os.ReadFile("templates/appproject.yaml")
	if err != nil {
		t.Fatalf("read AppProject template: %v", err)
	}
	content := string(manifest)

	for _, required := range []string{
		"helm.sh/hook: post-install,post-upgrade",
		"helm.sh/hook-weight: \"0\"",
		"helm.sh/hook-delete-policy: before-hook-creation",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("AppProject template is missing lifecycle ordering %q", required)
		}
	}
	if !strings.Contains(content, "kind: AppProject") {
		t.Fatal("AppProject template no longer renders an AppProject")
	}
	if !strings.Contains(content, ".Values.appProject.annotations") {
		t.Fatal("custom AppProject annotations were removed")
	}
}
