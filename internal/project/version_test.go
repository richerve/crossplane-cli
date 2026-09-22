/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package project

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	devv1alpha1 "github.com/crossplane/cli/v2/apis/dev/v1alpha1"
)

const testRepository = "xpkg.crossplane.io/example/test"

// hashProject returns the source hash of a single Directory-source function in
// a project whose functions directory holds the given files.
func hashProject(t *testing.T, files map[string]string) string {
	t.Helper()

	projFS := afero.NewMemMapFs()
	for path, content := range files {
		if err := afero.WriteFile(projFS, "functions/fn-one/"+path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	proj := &devv1alpha1.Project{Spec: devv1alpha1.ProjectSpec{Repository: testRepository}}
	proj.Default()

	fn := devv1alpha1.Function{
		Source:    devv1alpha1.FunctionSourceDirectory,
		Directory: &devv1alpha1.FunctionDirectory{Name: "fn-one"},
	}

	h, err := hashFunctionSource(projFS, proj, fn, "")
	if err != nil {
		t.Fatalf("hashFunctionSource: %v", err)
	}

	return h
}

func TestHashFunctionSource(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"main.k":  "a = 1\n",
		"kcl.mod": "[package]\nname = \"fn-one\"\n",
	}

	t.Run("StableAcrossCalls", func(t *testing.T) {
		t.Parallel()

		if diff := cmp.Diff(hashProject(t, base), hashProject(t, base)); diff != "" {
			t.Errorf("hash is not stable (-first +second):\n%s", diff)
		}
	})

	t.Run("ChangesWithContent", func(t *testing.T) {
		t.Parallel()

		changed := map[string]string{"main.k": "a = 2\n", "kcl.mod": base["kcl.mod"]}
		if hashProject(t, base) == hashProject(t, changed) {
			t.Error("hash did not change when the function's source changed")
		}
	})

	t.Run("ChangesWithFileSet", func(t *testing.T) {
		t.Parallel()

		extra := map[string]string{"main.k": base["main.k"], "kcl.mod": base["kcl.mod"], "helper.k": "b = 1\n"}
		if hashProject(t, base) == hashProject(t, extra) {
			t.Error("hash did not change when a file was added")
		}
	})

	t.Run("Length", func(t *testing.T) {
		t.Parallel()

		if got := len(hashProject(t, base)); got != functionVersionLength {
			t.Errorf("hash length = %d, want %d", got, functionVersionLength)
		}
	})

	// A developer's local virtualenv is excluded from the image the Python
	// builder produces, so it must not move the function's version either.
	t.Run("IgnoresPythonVenv", func(t *testing.T) {
		t.Parallel()

		py := map[string]string{"main.py": "x = 1\n", pythonProjectFile: "[project]\nname = \"fn-one\"\n"}
		withVenv := map[string]string{
			"main.py":                     py["main.py"],
			pythonProjectFile:             py[pythonProjectFile],
			pythonVenvDir + "/lib/pkg.py": "whatever\n",
		}
		if diff := cmp.Diff(hashProject(t, py), hashProject(t, withVenv)); diff != "" {
			t.Errorf("a local virtualenv changed the hash (-without +with):\n%s", diff)
		}
	})
}

// A Python function's generated schemas are staged from outside its directory
// at build time, so they have to be hashed from outside it too.
func TestHashFunctionSourcePythonSchemas(t *testing.T) {
	t.Parallel()

	hash := func(schema string) string {
		t.Helper()

		projFS := afero.NewMemMapFs()
		if err := afero.WriteFile(projFS, "functions/fn-one/main.py", []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(projFS, "functions/fn-one/"+pythonProjectFile, []byte("[project]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := afero.WriteFile(projFS, "schemas/python/models/xr.py", []byte(schema), 0o644); err != nil {
			t.Fatal(err)
		}

		proj := &devv1alpha1.Project{Spec: devv1alpha1.ProjectSpec{Repository: testRepository}}
		proj.Default()

		h, err := hashFunctionSource(projFS, proj, devv1alpha1.Function{
			Source:    devv1alpha1.FunctionSourceDirectory,
			Directory: &devv1alpha1.FunctionDirectory{Name: "fn-one"},
		}, "")
		if err != nil {
			t.Fatalf("hashFunctionSource: %v", err)
		}

		return h
	}

	if hash("class XR: pass\n") == hash("class XR:\n    spec = None\n") {
		t.Error("hash did not change when the generated python schemas changed")
	}
}

func TestFunctionRepository(t *testing.T) {
	t.Parallel()

	if diff := cmp.Diff(testRepository+"_fn-one", functionRepository(testRepository, "fn-one", "")); diff != "" {
		t.Errorf("unversioned repository (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(testRepository+"_fn-one-a1b2c3d4e5f6", functionRepository(testRepository, "fn-one", "a1b2c3d4e5f6")); diff != "" {
		t.Errorf("versioned repository (-want +got):\n%s", diff)
	}
}

func TestVersionFunctions(t *testing.T) {
	t.Parallel()

	projFS := afero.NewMemMapFs()
	if err := afero.WriteFile(projFS, "functions/fn-one/main.k", []byte("a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proj := &devv1alpha1.Project{Spec: devv1alpha1.ProjectSpec{Repository: testRepository}}
	proj.Default()

	versions, err := versionFunctions(projFS, proj, []devv1alpha1.Function{{
		Source:    devv1alpha1.FunctionSourceDirectory,
		Directory: &devv1alpha1.FunctionDirectory{Name: "fn-one"},
	}}, "")
	if err != nil {
		t.Fatalf("versionFunctions: %v", err)
	}

	v, ok := versions["fn-one"]
	if !ok {
		t.Fatalf("no version for fn-one; got %v", versions)
	}

	fn := devv1alpha1.Function{
		Source:    devv1alpha1.FunctionSourceDirectory,
		Directory: &devv1alpha1.FunctionDirectory{Name: "fn-one"},
	}
	version, err := hashFunctionSource(projFS, proj, fn, "")
	if err != nil {
		t.Fatal(err)
	}

	// The stable ref is what `crossplane function generate` writes into a
	// Composition, so it must not pick up the version. The versioned ref has to
	// match the repository path, because that is what Crossplane names the
	// Function object it installs for the dependency after.
	wantStable, err := functionRef(testRepository, "fn-one", "")
	if err != nil {
		t.Fatal(err)
	}
	wantRef, err := functionRef(testRepository, "fn-one", version)
	if err != nil {
		t.Fatal(err)
	}

	want := functionVersion{
		repo:      testRepository + "_fn-one-" + version,
		stableRef: wantStable,
		ref:       wantRef,
	}
	if diff := cmp.Diff(want, v, cmp.AllowUnexported(functionVersion{})); diff != "" {
		t.Errorf("version (-want +got):\n%s", diff)
	}
	if v.ref == v.stableRef {
		t.Error("versioned ref is the same as the stable ref")
	}
}

// A functionRef is a DNS label, cut at 63 characters. A repository long enough
// to push the version past the cut would name every version of a function the
// same, so the build has to refuse rather than produce it.
func TestVersionFunctionsRepositoryTooLong(t *testing.T) {
	t.Parallel()

	projFS := afero.NewMemMapFs()
	if err := afero.WriteFile(projFS, "functions/fn-one/main.k", []byte("a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proj := &devv1alpha1.Project{Spec: devv1alpha1.ProjectSpec{
		Repository: "xpkg.crossplane.io/an-organisation-with-a-long-name/a-project-with-a-long-name",
	}}
	proj.Default()

	fns := []devv1alpha1.Function{{
		Source:    devv1alpha1.FunctionSourceDirectory,
		Directory: &devv1alpha1.FunctionDirectory{Name: "fn-one"},
	}}

	// The unversioned name still fits, so this project builds fine today; it is
	// only versioning it that does not work.
	if _, err := versionFunctions(projFS, proj, fns, ""); err == nil {
		t.Error("versionFunctions accepted a repository too long to carry a version")
	}
}

// pipelineYAML returns a manifest of the given kind whose pipeline has one step
// bound to fnRef.
func pipelineYAML(apiVersion, kind, fnRef string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
  name: test
spec:
  pipeline:
  - step: one
    functionRef:
      name: %s
`, apiVersion, kind, fnRef)
}

// templatedPipelineYAML returns a CronOperation- or WatchOperation-shaped
// manifest, whose pipeline lives in the Operation it templates.
func templatedPipelineYAML(kind, fnRef string) string {
	return fmt.Sprintf(`apiVersion: ops.crossplane.io/v1alpha1
kind: %s
metadata:
  name: test
spec:
  operationTemplate:
    spec:
      pipeline:
      - step: one
        functionRef:
          name: %s
`, kind, fnRef)
}

func TestRewriteFunctionRefs(t *testing.T) {
	t.Parallel()

	versions := map[string]functionVersion{
		"fn-one": {
			repo:      testRepository + "_fn-one-a1b2c3d4e5f6",
			stableRef: "example-test-fn-one",
			ref:       "example-test-fn-one-a1b2c3d4e5f6",
		},
	}

	tcs := map[string]struct {
		file       string
		manifest   string
		wantUsed   bool
		wantRefs   []string
		wantUnread bool // the file should come back byte-for-byte unchanged
	}{
		"Composition": {
			file:     "comp.yaml",
			manifest: pipelineYAML("apiextensions.crossplane.io/v1", "Composition", "example-test-fn-one"),
			wantUsed: true,
			wantRefs: []string{"example-test-fn-one-a1b2c3d4e5f6"},
		},
		"Operation": {
			file:     "op.yaml",
			manifest: pipelineYAML("ops.crossplane.io/v1alpha1", "Operation", "example-test-fn-one"),
			wantUsed: true,
			wantRefs: []string{"example-test-fn-one-a1b2c3d4e5f6"},
		},
		"CronOperation": {
			file:     "cron.yaml",
			manifest: templatedPipelineYAML("CronOperation", "example-test-fn-one"),
			wantUsed: true,
			wantRefs: []string{"example-test-fn-one-a1b2c3d4e5f6"},
		},
		"WatchOperation": {
			file:     "watch.yaml",
			manifest: templatedPipelineYAML("WatchOperation", "example-test-fn-one"),
			wantUsed: true,
			wantRefs: []string{"example-test-fn-one-a1b2c3d4e5f6"},
		},
		// A step calling a function this project doesn't build - an upstream
		// package, say - has no version to point at and must be left alone.
		"ForeignFunction": {
			file:       "comp.yaml",
			manifest:   pipelineYAML("apiextensions.crossplane.io/v1", "Composition", "crossplane-contrib-function-auto-ready"),
			wantUsed:   false,
			wantRefs:   []string{"crossplane-contrib-function-auto-ready"},
			wantUnread: true,
		},
		"UnrelatedKind": {
			file:       "xrd.yaml",
			manifest:   xrdYAML("acme.example.com", "xdatabases", "xdatabase", "XDatabase"),
			wantUsed:   false,
			wantUnread: true,
		},
	}

	for name, tc := range tcs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			packageFS := afero.NewMemMapFs()
			if err := afero.WriteFile(packageFS, "/"+tc.file, []byte(tc.manifest), 0o644); err != nil {
				t.Fatal(err)
			}

			used, err := rewriteFunctionRefs(packageFS, versions)
			if err != nil {
				t.Fatalf("rewriteFunctionRefs: %v", err)
			}
			if diff := cmp.Diff(tc.wantUsed, used["fn-one"]); diff != "" {
				t.Errorf("used (-want +got):\n%s", diff)
			}

			out, err := afero.ReadFile(packageFS, "/"+tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantUnread {
				if diff := cmp.Diff(tc.manifest, string(out)); diff != "" {
					t.Errorf("file was rewritten but should not have been (-want +got):\n%s", diff)
				}
			}
			if tc.wantRefs != nil {
				if diff := cmp.Diff(tc.wantRefs, functionRefsIn(t, out)); diff != "" {
					t.Errorf("functionRefs (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestRewriteFunctionRefsPreservesUnknownFields guards the round-trip through a
// map: a field the CLI doesn't model must survive the rewrite.
func TestRewriteFunctionRefsPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	manifest := `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: test
spec:
  someFutureField: keep-me
  pipeline:
  - step: one
    functionRef:
      name: example-test-fn-one
    credentials:
    - name: creds
      source: Secret
`
	packageFS := afero.NewMemMapFs()
	if err := afero.WriteFile(packageFS, "/comp.yaml", []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := rewriteFunctionRefs(packageFS, map[string]functionVersion{
		"fn-one": {stableRef: "example-test-fn-one", ref: "example-test-fn-one-a1b2c3d4e5f6"},
	}); err != nil {
		t.Fatalf("rewriteFunctionRefs: %v", err)
	}

	out, err := afero.ReadFile(packageFS, "/comp.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var comp map[string]any
	if err := yaml.Unmarshal(out, &comp); err != nil {
		t.Fatal(err)
	}

	spec, _ := comp["spec"].(map[string]any)
	if diff := cmp.Diff("keep-me", spec["someFutureField"]); diff != "" {
		t.Errorf("unknown field (-want +got):\n%s", diff)
	}

	step, _ := spec["pipeline"].([]any)[0].(map[string]any)
	if _, ok := step["credentials"]; !ok {
		t.Error("step credentials were dropped")
	}
}

// functionRefsIn returns every functionRef name in a manifest, whichever kind
// of pipeline holds it.
func functionRefsIn(t *testing.T, manifest []byte) []string {
	t.Helper()

	var tm metav1.TypeMeta
	if err := yaml.Unmarshal(manifest, &tm); err != nil {
		t.Fatal(err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		t.Fatal(err)
	}

	pipeline := pipelineOf(tm, obj)
	refs := make([]string, 0, len(pipeline))
	for _, s := range pipeline {
		step, _ := s.(map[string]any)
		ref, _ := step["functionRef"].(map[string]any)
		name, _ := ref["name"].(string)
		refs = append(refs, name)
	}

	return refs
}
