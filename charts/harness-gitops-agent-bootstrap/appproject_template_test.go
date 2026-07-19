package bootstrap

import (
	"os"
	"strings"
	"testing"
)

func TestAppProjectIsStableHelmManagedResource(t *testing.T) {
	manifest, err := os.ReadFile("templates/appproject.yaml")
	if err != nil {
		t.Fatalf("read AppProject template: %v", err)
	}
	content := string(manifest)

	for _, forbidden := range []string{
		"helm.sh/hook",
		"helm.sh/hook-weight",
		"helm.sh/hook-delete-policy",
		"post-upgrade",
		"before-hook-creation",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("AppProject template still contains lifecycle hook %q", forbidden)
		}
	}
	if !strings.Contains(content, "kind: AppProject") {
		t.Fatal("AppProject template no longer renders an AppProject")
	}
	if !strings.Contains(content, ".Values.appProject.annotations") {
		t.Fatal("custom AppProject annotations were removed with the Helm hook annotations")
	}
}
