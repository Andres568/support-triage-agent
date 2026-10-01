package providers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const terraformDir = "../../infra/terraform"

// TestTerraformModelsMatchRegistry fails when infra/terraform's model map or
// the model ids its tests plan with drift from the registry: a var.model the
// worker cannot Lookup would pass `terraform test` and fail at startup.
func TestTerraformModelsMatchRegistry(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(terraformDir, "models.tf"))
	if err != nil {
		t.Fatal(err)
	}
	entries := regexp.MustCompile(`(?m)^\s*"([^"]+)"\s*=\s*"([a-z_]+)"\s*$`).FindAllStringSubmatch(string(src), -1)
	if len(entries) == 0 {
		t.Fatal("no model_providers entries parsed from models.tf")
	}
	for _, e := range entries {
		id, slug := e[1], e[2]
		spec, err := Lookup(id)
		if err != nil {
			t.Errorf("models.tf: %v", err)
			continue
		}
		if want := strings.ToUpper(slug) + "_API_KEY"; spec.KeyEnv != want {
			t.Errorf("models.tf: %s -> %q mounts %s, the registry reads %q", id, slug, want, spec.KeyEnv)
		}
	}

	tests, err := filepath.Glob(filepath.Join(terraformDir, "tests", "*.tftest.hcl"))
	if err != nil || len(tests) == 0 {
		t.Fatalf("no tftest files: %v", err)
	}
	modelVar := regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)
	for _, f := range tests {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range strings.Split(string(b), "\nrun ") {
			if strings.Contains(run, "expect_failures = [var.model]") {
				continue // deliberately invalid
			}
			for _, m := range modelVar.FindAllStringSubmatch(run, -1) {
				if _, err := Lookup(m[1]); err != nil {
					t.Errorf("%s: %v", filepath.Base(f), err)
				}
			}
		}
	}
}
