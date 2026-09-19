package installcheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var commitActionPattern = regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)

func isImmutableActionReference(ref string) bool {
	return strings.HasPrefix(ref, "./") || commitActionPattern.MatchString(ref)
}

func TestActionReferenceClassification(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		ref  string
		want bool
	}{
		{name: "commit pinned action", ref: "actions/checkout@" + sha, want: true},
		{name: "commit pinned nested action", ref: "frostyard/repogen/.github/actions/publish-to-r2@" + sha, want: true},
		{name: "local action", ref: "./.github/actions/check", want: true},
		{name: "version tag", ref: "actions/checkout@v6", want: false},
		{name: "branch", ref: "frostyard/repogen/.github/actions/publish-to-r2@main", want: false},
		{name: "short commit", ref: "actions/checkout@deadbeef", want: false},
		{name: "expression", ref: "actions/checkout@${{inputs.ref}}", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isImmutableActionReference(tc.ref); got != tc.want {
				t.Errorf("isImmutableActionReference(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestWorkflowActionsUseImmutableCommitSHAs(t *testing.T) {
	workflowDir := filepath.Join(RepoRoot(), ".github", "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read workflow directory: %v", err)
	}

	workflowCount := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		workflowCount++

		path := filepath.Join(workflowDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		var document yaml.Node
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Errorf("parse %s: %v", path, err)
			continue
		}

		var inspectUses func(*yaml.Node)
		inspectUses = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					key, value := node.Content[i], node.Content[i+1]
					if key.Kind == yaml.ScalarNode && key.Tag == "!!str" && key.Value == "uses" {
						if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
							t.Errorf("%s:%d: uses value must be a string", entry.Name(), value.Line)
						} else if !isImmutableActionReference(value.Value) {
							t.Errorf("%s:%d: external action %q must use a full 40-character commit SHA", entry.Name(), value.Line, value.Value)
						}
					}
					inspectUses(value)
				}
				return
			}
			for _, child := range node.Content {
				inspectUses(child)
			}
		}
		inspectUses(&document)
	}
	if workflowCount == 0 {
		t.Fatal("no workflow files found")
	}
}

func TestWorkflowUsesLeastPrivilege(t *testing.T) {
	path := filepath.Join(".github", "workflows", "test.yml")
	workflow := readRepoFile(t, path)

	var config struct {
		Permissions *map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &config); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if config.Permissions == nil || len(*config.Permissions) != 0 {
		t.Errorf("top-level permissions = %v, want explicit empty permissions", config.Permissions)
	}

	expected := map[string]map[string]string{
		"lint":      {"contents": "read"},
		"unit-test": {"contents": "read", "id-token": "write"},
		"race-test": {"contents": "read"},
		"e2e":       {"contents": "read"},
		"verify":    {"contents": "read"},
		"build":     {"contents": "read"},
	}
	if len(config.Jobs) != len(expected) {
		t.Errorf("workflow has %d jobs, want %d", len(config.Jobs), len(expected))
	}
	for name, want := range expected {
		job, ok := config.Jobs[name]
		if !ok {
			t.Errorf("workflow does not define %s job", name)
			continue
		}
		if len(job.Permissions) != len(want) {
			t.Errorf("%s permissions = %v, want %v", name, job.Permissions, want)
			continue
		}
		for permission, access := range want {
			if job.Permissions[permission] != access {
				t.Errorf("%s permissions = %v, want %v", name, job.Permissions, want)
				break
			}
		}
	}
}

func TestReleaseWorkflowGatedOnRequiredChecks(t *testing.T) {
	path := filepath.Join(".github", "workflows", "release.yml")
	workflow := readRepoFile(t, path)

	var config struct {
		Permissions *map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Needs       any               `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &config); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if config.Permissions == nil || len(*config.Permissions) != 0 {
		t.Errorf("top-level permissions = %v, want explicit empty permissions", config.Permissions)
	}

	gateJob, ok := config.Jobs["gate"]
	if !ok {
		t.Fatal("release.yml must define a 'gate' job")
	}
	if len(gateJob.Permissions) != 1 || gateJob.Permissions["contents"] != "read" {
		t.Errorf("gate job permissions = %v, want contents: read", gateJob.Permissions)
	}
	gateRunsMakeCI := false
	for _, step := range gateJob.Steps {
		if strings.Contains(step.Run, "make ci") {
			gateRunsMakeCI = true
			break
		}
	}
	if !gateRunsMakeCI {
		t.Errorf("gate job steps must execute 'make ci'")
	}

	e2eJob, ok := config.Jobs["e2e"]
	if !ok {
		t.Fatal("release.yml must define an 'e2e' job")
	}
	if len(e2eJob.Permissions) != 1 || e2eJob.Permissions["contents"] != "read" {
		t.Errorf("e2e job permissions = %v, want contents: read", e2eJob.Permissions)
	}
	e2eRunsMakeE2E := false
	for _, step := range e2eJob.Steps {
		if strings.Contains(step.Run, "make e2e") {
			e2eRunsMakeE2E = true
			break
		}
	}
	if !e2eRunsMakeE2E {
		t.Errorf("e2e job steps must execute 'make e2e'")
	}

	goreleaserJob, ok := config.Jobs["goreleaser"]
	if !ok {
		t.Fatal("release.yml must define a 'goreleaser' job")
	}
	if len(goreleaserJob.Permissions) != 1 || goreleaserJob.Permissions["contents"] != "write" {
		t.Errorf("goreleaser job permissions = %v, want contents: write", goreleaserJob.Permissions)
	}

	var needsList []string
	switch v := goreleaserJob.Needs.(type) {
	case string:
		needsList = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				needsList = append(needsList, s)
			}
		}
	}
	hasGate := false
	hasE2E := false
	for _, n := range needsList {
		if n == "gate" {
			hasGate = true
		}
		if n == "e2e" {
			hasE2E = true
		}
	}
	if !hasGate || !hasE2E {
		t.Errorf("goreleaser job needs = %v, want [gate, e2e]", needsList)
	}
}
