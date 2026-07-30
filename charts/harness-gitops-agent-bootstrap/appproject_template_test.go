package bootstrap

import (
	"os"
	"strings"
	"testing"
)

func TestAppProjectIsManagedAsNormalHelmResource(t *testing.T) {
	manifest, err := os.ReadFile("templates/appproject.yaml")
	if err != nil {
		t.Fatalf("read AppProject template: %v", err)
	}
	content := string(manifest)

	for _, forbidden := range []string{
		"helm.sh/hook:",
		"helm.sh/hook-weight:",
		"helm.sh/hook-delete-policy:",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("AppProject must not be rendered as an untracked Helm hook: %q", forbidden)
		}
	}
	if !strings.Contains(content, "kind: AppProject") {
		t.Fatal("AppProject template no longer renders an AppProject")
	}
	if !strings.Contains(content, ".Values.appProject.annotations") {
		t.Fatal("custom AppProject annotations were removed")
	}
	if !strings.Contains(content, `.Capabilities.APIVersions.Has "argoproj.io/v1alpha1/AppProject"`) {
		t.Fatal("AppProject template must fail before rendering when its CRD is unavailable")
	}
}
