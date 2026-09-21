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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	devv1alpha1 "github.com/crossplane/cli/v2/apis/dev/v1alpha1"
	"github.com/crossplane/cli/v2/internal/project/functions"
)

const testRepo = "xpkg.example.com/acme/proj"

// testRef is the functionRef that `function generate` writes for a function
// named "my-fn" in a project with testRepo as its repository. ToDNSLabel drops
// the underscore that separates the repository from the function name rather
// than replacing it, hence "projmy-fn".
const testRef = "acme-projmy-fn"

func composition(steps ...string) string {
	return "apiVersion: apiextensions.crossplane.io/v1\n" +
		"kind: Composition\n" +
		"metadata:\n  name: test\n" +
		"spec:\n  mode: Pipeline\n  pipeline:\n" +
		strings.Join(steps, "")
}

func step(name, ref string) string {
	return "  - step: " + name + "\n    functionRef:\n      name: " + ref + "\n"
}

func testProject() *devv1alpha1.Project {
	return &devv1alpha1.Project{
		Spec: devv1alpha1.ProjectSpec{
			Repository: testRepo,
			Paths:      &devv1alpha1.ProjectPaths{Functions: "functions"},
		},
	}
}

func inlineFn(name string) devv1alpha1.Function {
	return devv1alpha1.Function{
		Source:    devv1alpha1.FunctionSourceDirectory,
		Directory: &devv1alpha1.FunctionDirectory{Name: name},
		Inline:    true,
	}
}

func TestInlineFunctions(t *testing.T) {
	t.Parallel()

	tcs := map[string]struct {
		projectFiles map[string]string
		packageFiles map[string]string
		fns          []devv1alpha1.Function

		wantErr      string
		wantSource   string
		wantDeps     string
		wantRewrites int
	}{
		"SingleFileNoDependencies": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "items = []\n",
			wantRewrites: 1,
		},
		"MainFirstThenOtherFilesSorted": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "items = [_a, _b]\n",
				"functions/my-fn/zz.k":    "_b = 2\n",
				"functions/my-fn/aa.k":    "_a = 1\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "items = [_a, _b]\n\n_a = 1\n\n_b = 2\n",
			wantRewrites: 1,
		},
		"TestFilesAreExcluded": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod":        "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":         "items = []\n",
				"functions/my-fn/main_test.k":    "import testdata\ntest_items = testdata.want\n",
				"functions/my-fn/testdata/fix.k": "want = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "items = []\n",
			wantRewrites: 1,
		},
		"OnlyTestFilesIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod":     "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main_test.k": "test_a = 1\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "no .k files found",
		},
		"ExactRegistryDependencyIsTranslated": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n\n[dependencies]\nk8s = \"1.31\"\n",
				"functions/my-fn/main.k":  "import k8s.api.core.v1 as k8core\nitems = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "import k8s.api.core.v1 as k8core\nitems = []\n",
			wantDeps:     "k8s = \"1.31\"",
			wantRewrites: 1,
		},
		"RewritesEveryMatchingStepAcrossCompositions": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/a.yaml": composition(step("auto-ready", "crossplane-contrib-function-auto-ready"), step("my-fn", testRef)),
				"/apis/b.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "items = []\n",
			wantRewrites: 2,
		},
		"LocalPathDependencyIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n\n[dependencies]\nmodels = { path = \"./model\" }\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "local path dependency",
		},
		"VersionRangeIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n\n[dependencies]\nk8s = \">=1.31\"\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "must pin dependencies exactly",
		},
		"LocalSubpackageImportIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod":            "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":             "import composition\nitems = composition.render()\n",
				"functions/my-fn/composition/main.k": "render = lambda -> any { [] }\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "only the function's top-level package is inlined",
		},
		"ExplicitlyRelativeImportIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "import .helpers\nitems = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "only the function's top-level package is inlined",
		},
		"RegistryImportIsNotMistakenForLocal": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n\n[dependencies]\nk8s = \"1.36\"\n",
				"functions/my-fn/main.k":  "import k8s.api.core.v1 as k8core\nitems = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:          []devv1alpha1.Function{inlineFn("my-fn")},
			wantSource:   "import k8s.api.core.v1 as k8core\nitems = []\n",
			wantDeps:     "k8s = \"1.36\"",
			wantRewrites: 1,
		},
		"NonKCLFunctionIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/go.mod": "module example.com/my-fn\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "only KCL functions can be inlined",
		},
		"TarballSourceIsRejected": {
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("my-fn", testRef)),
			},
			fns: []devv1alpha1.Function{{
				Source:  devv1alpha1.FunctionSourceTarball,
				Tarball: &devv1alpha1.FunctionTarball{Name: "my-fn"},
				Inline:  true,
			}},
			wantErr: "cannot be inlined",
		},
		"UnreferencedFunctionIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(step("other", "something-else")),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "no Composition pipeline step references",
		},
		"StepWithExistingInputIsRejected": {
			projectFiles: map[string]string{
				"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
				"functions/my-fn/main.k":  "items = []\n",
			},
			packageFiles: map[string]string{
				"/apis/composition.yaml": composition(
					step("my-fn", testRef) + "    input:\n      apiVersion: example.org/v1\n      kind: Thing\n",
				),
			},
			fns:     []devv1alpha1.Function{inlineFn("my-fn")},
			wantErr: "already sets input",
		},
	}

	for name, tc := range tcs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			projectFS := afero.NewMemMapFs()
			for p, c := range tc.projectFiles {
				if err := afero.WriteFile(projectFS, p, []byte(c), 0o644); err != nil {
					t.Fatalf("failed to write project file %q: %v", p, err)
				}
			}
			packageFS := afero.NewMemMapFs()
			for p, c := range tc.packageFiles {
				if err := afero.WriteFile(packageFS, p, []byte(c), 0o644); err != nil {
					t.Fatalf("failed to write package file %q: %v", p, err)
				}
			}

			deps, err := inlineFunctions(packageFS, projectFS, testProject(), tc.fns)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(deps) != 1 {
				t.Fatalf("expected 1 dependency, got %d", len(deps))
			}
			if got := *deps[0].Package; got != functions.KCLRuntimePackage {
				t.Errorf("expected dependency on %q, got %q", functions.KCLRuntimePackage, got)
			}
			if deps[0].Version != functions.KCLRuntimeVersion {
				t.Errorf("expected version %q, got %q", functions.KCLRuntimeVersion, deps[0].Version)
			}

			rewrites := 0
			for p := range tc.packageFiles {
				bs, err := afero.ReadFile(packageFS, p)
				if err != nil {
					t.Fatalf("failed to read %q: %v", p, err)
				}

				var comp map[string]any
				if err := yaml.Unmarshal(bs, &comp); err != nil {
					t.Fatalf("failed to parse %q: %v", p, err)
				}

				pipeline, _ := comp["spec"].(map[string]any)["pipeline"].([]any)
				for _, s := range pipeline {
					st := s.(map[string]any)
					input, ok := st["input"].(map[string]any)
					if !ok {
						continue
					}
					rewrites++

					ref := st["functionRef"].(map[string]any)["name"]
					if ref != functions.KCLRuntimeFunctionName {
						t.Errorf("expected functionRef %q, got %q", functions.KCLRuntimeFunctionName, ref)
					}
					if got := input["apiVersion"]; got != kclInputAPIVersion {
						t.Errorf("expected input apiVersion %q, got %q", kclInputAPIVersion, got)
					}
					if got := input["kind"]; got != kclInputKind {
						t.Errorf("expected input kind %q, got %q", kclInputKind, got)
					}

					inputSpec := input["spec"].(map[string]any)
					if diff := cmp.Diff(tc.wantSource, inputSpec["source"]); diff != "" {
						t.Errorf("inlined source: -want +got:\n%s", diff)
					}

					gotDeps, _ := inputSpec["dependencies"].(string)
					if diff := cmp.Diff(tc.wantDeps, gotDeps); diff != "" {
						t.Errorf("inlined dependencies: -want +got:\n%s", diff)
					}
				}
			}

			if rewrites != tc.wantRewrites {
				t.Errorf("expected %d rewritten steps, got %d", tc.wantRewrites, rewrites)
			}
		})
	}
}

// TestInlineFunctionsPreservesUnknownFields guards the round-trip: a
// Composition is rewritten through a map so that fields the CLI does not model
// survive into the built package.
func TestInlineFunctionsPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	projectFS := afero.NewMemMapFs()
	for p, c := range map[string]string{
		"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
		"functions/my-fn/main.k":  "items = []\n",
	} {
		if err := afero.WriteFile(projectFS, p, []byte(c), 0o644); err != nil {
			t.Fatalf("failed to write %q: %v", p, err)
		}
	}

	comp := composition(step("my-fn", testRef)) +
		"  writeConnectionSecretsToNamespace: tests\n" +
		"  someFutureField:\n    nested: value\n"

	packageFS := afero.NewMemMapFs()
	if err := afero.WriteFile(packageFS, "/apis/composition.yaml", []byte(comp), 0o644); err != nil {
		t.Fatalf("failed to write composition: %v", err)
	}

	if _, err := inlineFunctions(packageFS, projectFS, testProject(), []devv1alpha1.Function{inlineFn("my-fn")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bs, err := afero.ReadFile(packageFS, "/apis/composition.yaml")
	if err != nil {
		t.Fatalf("failed to read composition: %v", err)
	}

	var got map[string]any
	if err := yaml.Unmarshal(bs, &got); err != nil {
		t.Fatalf("failed to parse composition: %v", err)
	}

	spec := got["spec"].(map[string]any)
	if diff := cmp.Diff("tests", spec["writeConnectionSecretsToNamespace"]); diff != "" {
		t.Errorf("known field lost: -want +got:\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{"nested": "value"}, spec["someFutureField"]); diff != "" {
		t.Errorf("unknown field lost: -want +got:\n%s", diff)
	}
}

// TestInlineFunctionsIgnoresNonCompositions checks that other resources staged
// for packaging are left byte-identical.
func TestInlineFunctionsIgnoresNonCompositions(t *testing.T) {
	t.Parallel()

	projectFS := afero.NewMemMapFs()
	for p, c := range map[string]string{
		"functions/my-fn/kcl.mod": "[package]\nname = \"my-fn\"\n",
		"functions/my-fn/main.k":  "items = []\n",
	} {
		if err := afero.WriteFile(projectFS, p, []byte(c), 0o644); err != nil {
			t.Fatalf("failed to write %q: %v", p, err)
		}
	}

	xrd := "apiVersion: apiextensions.crossplane.io/v2\nkind: CompositeResourceDefinition\nmetadata:\n  name: xbuckets.example.org\n"

	packageFS := afero.NewMemMapFs()
	if err := afero.WriteFile(packageFS, "/apis/composition.yaml", []byte(composition(step("my-fn", testRef))), 0o644); err != nil {
		t.Fatalf("failed to write composition: %v", err)
	}
	if err := afero.WriteFile(packageFS, "/apis/xrd.yaml", []byte(xrd), 0o644); err != nil {
		t.Fatalf("failed to write xrd: %v", err)
	}

	if _, err := inlineFunctions(packageFS, projectFS, testProject(), []devv1alpha1.Function{inlineFn("my-fn")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := afero.ReadFile(packageFS, "/apis/xrd.yaml")
	if err != nil {
		t.Fatalf("failed to read xrd: %v", err)
	}
	if diff := cmp.Diff(xrd, string(got)); diff != "" {
		t.Errorf("XRD was modified: -want +got:\n%s", diff)
	}
}

// TestBuilderBuildInlineFunction exercises inlining through the whole build:
// the inlined function must not produce a function package image, and the
// Composition inside the built Configuration must carry its source.
func TestBuilderBuildInlineFunction(t *testing.T) {
	t.Parallel()

	projFS := afero.NewMemMapFs()
	writeProject(t, projFS,
		map[string]string{
			"db.yaml": xrdYAML("acme.example.com", "xdatabases", "xdatabase", "XDatabase"),
			"db-comp.yaml": "apiVersion: apiextensions.crossplane.io/v1\n" +
				"kind: Composition\n" +
				"metadata:\n  name: xdb\n" +
				"spec:\n  compositeTypeRef:\n    apiVersion: acme.example.com/v1alpha1\n    kind: XDatabase\n" +
				"  mode: Pipeline\n  pipeline:\n" +
				"  - step: fn-inline\n    functionRef:\n      name: examplefn-inline\n" +
				"  - step: fn-packaged\n    functionRef:\n      name: examplefn-packaged\n",
		},
		[]string{"fn-inline", "fn-packaged"},
	)

	for path, content := range map[string]string{
		"functions/fn-inline/kcl.mod": "[package]\nname = \"fn-inline\"\n",
		"functions/fn-inline/main.k":  "items = []\n",
	} {
		if err := afero.WriteFile(projFS, path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	proj := &devv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "test-project"},
		Spec: devv1alpha1.ProjectSpec{
			Repository: "xpkg.crossplane.io/example",
			Functions: []devv1alpha1.Function{
				{
					Source:    devv1alpha1.FunctionSourceDirectory,
					Directory: &devv1alpha1.FunctionDirectory{Name: "fn-inline"},
					Inline:    true,
				},
				{
					Source:    devv1alpha1.FunctionSourceDirectory,
					Directory: &devv1alpha1.FunctionDirectory{Name: "fn-packaged"},
				},
			},
		},
	}
	proj.Default()

	b := NewBuilder(BuildWithFunctionIdentifier(functions.FakeIdentifier))

	imgMap, err := b.Build(t.Context(), proj, projFS)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The inlined function must not be built or pushed as its own package.
	want := map[string]bool{
		proj.Spec.Repository:                  true,
		proj.Spec.Repository + "_fn-packaged": true,
	}
	got := map[string]bool{}
	for tag := range imgMap {
		got[tag.Repository.Name()] = true
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Build(...) function repos: -want, +got:\n%s", diff)
	}
}
