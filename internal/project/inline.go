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
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xpkg"

	xpv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	xpmetav1 "github.com/crossplane/crossplane/apis/v2/pkg/meta/v1"
	xpkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"

	devv1alpha1 "github.com/crossplane/cli/v2/apis/dev/v1alpha1"
	"github.com/crossplane/cli/v2/internal/project/functions"
)

const (
	// kclInputAPIVersion and kclInputKind identify the input resource that
	// function-kcl reads its source and dependencies from.
	kclInputAPIVersion = "krm.kcl.dev/v1alpha1"
	kclInputKind       = "KCLInput"

	// kclModFile is the KCL module manifest, present in every KCL function
	// directory.
	kclModFile = "kcl.mod"

	// kclTestSuffix identifies a KCL test file. Tests are not part of the
	// function's runtime behaviour, so they are left out of inlined source.
	kclTestSuffix = "_test.k"
)

// inlinedFunction is the source of a single function, extracted from the
// project and ready to be written into a pipeline step.
type inlinedFunction struct {
	// name of the function, as declared in the project.
	name string
	// ref is the functionRef name that pipeline steps use to reach this
	// function when it is built as its own package. Steps matching this ref
	// are the ones we rewrite.
	ref string
	// source is the function's KCL code, as a single module.
	source string
	// dependencies is the body of the function's kcl.mod [dependencies]
	// table, in TOML, or empty when the function has none.
	dependencies string
}

// inlineFunctions rewrites the pipeline steps of Compositions in packageFS
// that reference an inlined function. Each matching step is repointed at the
// generic function-kcl runtime and given the function's source as its input,
// so the code travels with the Composition instead of living in a separate
// function package.
//
// It returns the package dependencies the rewritten Compositions need, which
// the caller adds to the Configuration's metadata. Compositions in packageFS
// are modified in place; the project's own files are never touched.
func inlineFunctions(packageFS, projectFS afero.Fs, project *devv1alpha1.Project, fns []devv1alpha1.Function) ([]xpmetav1.Dependency, error) {
	if len(fns) == 0 {
		return nil, nil
	}

	inlined := make(map[string]inlinedFunction, len(fns))
	for _, fn := range fns {
		f, err := loadInlineFunction(projectFS, project, fn)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot inline function %q", fn.Name())
		}
		inlined[f.ref] = f
	}

	used, err := rewriteCompositions(packageFS, inlined)
	if err != nil {
		return nil, err
	}

	// A function nobody calls is almost always a mistake: either the step was
	// never added to a Composition, or the project's repository changed after
	// the step was generated, so the functionRef no longer matches. Either way
	// the built package would silently omit the function's logic.
	for ref, f := range inlined {
		if !used[ref] {
			return nil, errors.Errorf("function %q is marked inline but no Composition pipeline step references %q; add the step or remove the inline field", f.name, ref)
		}
	}

	return []xpmetav1.Dependency{{
		APIVersion: new(xpkgv1.FunctionGroupVersionKind.GroupVersion().String()),
		Kind:       new(xpkgv1.FunctionKind),
		Package:    new(functions.KCLRuntimePackage),
		Version:    functions.KCLRuntimeVersion,
	}}, nil
}

// loadInlineFunction reads a function's source out of the project and checks
// that it can stand alone inside a Composition.
func loadInlineFunction(projectFS afero.Fs, project *devv1alpha1.Project, fn devv1alpha1.Function) (inlinedFunction, error) {
	fnName := fn.Name()

	if fn.Source != devv1alpha1.FunctionSourceDirectory {
		return inlinedFunction{}, errors.Errorf("source %q cannot be inlined; only %q supplies the source code inlining needs", fn.Source, devv1alpha1.FunctionSourceDirectory)
	}

	fnFS := afero.NewBasePathFs(projectFS, filepath.Join(project.Spec.Paths.Functions, fnName))

	// kcl.mod is how the KCL builder recognises a KCL function, so its absence
	// means this is a function in some other language.
	mod, err := afero.ReadFile(fnFS, kclModFile)
	if err != nil {
		return inlinedFunction{}, errors.Errorf("only KCL functions can be inlined, and no %s was found", kclModFile)
	}

	deps, err := inlineDependencies(mod)
	if err != nil {
		return inlinedFunction{}, err
	}

	source, err := concatKCLSource(fnFS)
	if err != nil {
		return inlinedFunction{}, err
	}

	if err := checkLocalImports(fnFS, source); err != nil {
		return inlinedFunction{}, err
	}

	ref, err := functionRefName(project.Spec.Repository, fnName)
	if err != nil {
		return inlinedFunction{}, err
	}

	return inlinedFunction{
		name:         fnName,
		ref:          ref,
		source:       source,
		dependencies: deps,
	}, nil
}

// functionRefName returns the functionRef that pipeline steps use to reach an
// embedded function. It must match the name `crossplane function generate`
// writes into the Composition when it adds the step.
func functionRefName(repository, fnName string) (string, error) {
	repo, err := name.NewRepository(fmt.Sprintf("%s_%s", repository, fnName))
	if err != nil {
		return "", errors.Wrapf(err, "cannot build function reference from repository %q", repository)
	}

	return xpkg.ToDNSLabel(repo.RepositoryStr()), nil
}

// kclMod is the subset of kcl.mod we care about when inlining.
type kclMod struct {
	Dependencies map[string]toml.Primitive `toml:"dependencies"`
}

// inlineDependencies translates a function's kcl.mod [dependencies] table into
// the TOML body that function-kcl expects in its input.
//
// Two kinds of dependency are rejected. A local path dependency - which is how
// the CLI wires up generated schemas - has no meaning inside the function pod,
// since only the function's own source is inlined. A dependency without an
// exact version defeats the point of inlining: two CompositionRevisions built
// from the same source could resolve it differently, so the revision would no
// longer pin what it runs.
func inlineDependencies(mod []byte) (string, error) {
	var m kclMod
	md, err := toml.Decode(string(mod), &m)
	if err != nil {
		return "", errors.Wrapf(err, "cannot parse %s", kclModFile)
	}

	names := make([]string, 0, len(m.Dependencies))
	for n := range m.Dependencies {
		names = append(names, n)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, n := range names {
		// A dependency is either a bare version string or a table of
		// coordinates; anything else is not something we can reason about.
		var version string
		if err := md.PrimitiveDecode(m.Dependencies[n], &version); err == nil {
			if err := checkExactVersion(n, version); err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("%s = %q", n, version))
			continue
		}

		var table map[string]any
		if err := md.PrimitiveDecode(m.Dependencies[n], &table); err != nil {
			return "", errors.Wrapf(err, "cannot parse dependency %q", n)
		}

		if _, ok := table["path"]; ok {
			return "", errors.Errorf("dependency %q is a local path dependency, which cannot be inlined because only the function's own source is embedded in the Composition; remove the dependency or build this function as a package", n)
		}

		version, _ = table["version"].(string)
		if err := checkExactVersion(n, version); err != nil {
			return "", err
		}

		parts := make([]string, 0, len(table))
		for _, k := range sortedKeys(table) {
			parts = append(parts, fmt.Sprintf("%s = %q", k, fmt.Sprint(table[k])))
		}
		lines = append(lines, fmt.Sprintf("%s = { %s }", n, strings.Join(parts, ", ")))
	}

	return strings.Join(lines, "\n"), nil
}

// checkExactVersion rejects version constraints that could resolve differently
// between builds.
func checkExactVersion(dep, version string) error {
	if version == "" {
		return errors.Errorf("dependency %q has no version; inlined functions must pin dependencies exactly so every CompositionRevision resolves the same code", dep)
	}
	if strings.ContainsAny(version, "<>=^~*") {
		return errors.Errorf("dependency %q has version constraint %q; inlined functions must pin dependencies exactly so every CompositionRevision resolves the same code", dep, version)
	}

	return nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)

	return ks
}

// concatKCLSource joins every .k file in a function directory into a single
// KCL module.
//
// Files in one directory form a single KCL package and share a scope without
// importing each other, so concatenating them preserves their meaning. Any
// symbol collision that would break the result already breaks the module
// today, because KCL rejects duplicate declarations across a package.
func concatKCLSource(fnFS afero.Fs) (string, error) {
	entries, err := afero.ReadDir(fnFS, ".")
	if err != nil {
		return "", errors.Wrap(err, "cannot read function directory")
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".k" {
			continue
		}
		// Test files are part of the same package as the code they test, so
		// they would otherwise be concatenated into the inlined source along
		// with whatever fixtures they import. KCL collects tests by this
		// suffix, so it is the language's own definition of what is a test.
		if strings.HasSuffix(e.Name(), kclTestSuffix) {
			continue
		}
		files = append(files, e.Name())
	}

	if len(files) == 0 {
		return "", errors.New("no .k files found")
	}

	// main.k leads, so the inlined source reads the way the function does on
	// disk. The rest follow in a stable order so a build is reproducible.
	sort.Strings(files)
	if i := slices.Index(files, "main.k"); i > 0 {
		files = slices.Insert(slices.Delete(files, i, i+1), 0, "main.k")
	}

	parts := make([]string, 0, len(files))
	for _, f := range files {
		bs, err := afero.ReadFile(fnFS, f)
		if err != nil {
			return "", errors.Wrapf(err, "cannot read %q", f)
		}
		parts = append(parts, strings.TrimRight(string(bs), "\n"))
	}

	return strings.Join(parts, "\n\n") + "\n", nil
}

// kclImport matches a KCL import statement, capturing the package path. The
// path may be dotted ("k8s.api.core.v1") and may be prefixed with dots to
// make it explicitly relative (".composition").
var kclImport = regexp.MustCompile(`(?m)^\s*import\s+(\.*[\w.]+)`)

// checkLocalImports rejects source that imports another package from inside
// the function's own directory.
//
// Subdirectories of a KCL module are separate packages, reached with imports
// like "import composition". Only the function's top-level package is inlined,
// so those imports would not resolve in the function pod. Flattening them
// would mean rewriting every qualified reference to a new name, which needs a
// KCL parser rather than text handling.
//
// This has to be caught here. The build would otherwise succeed and produce a
// Composition that fails when Crossplane runs it.
func checkLocalImports(fnFS afero.Fs, source string) error {
	local := make([]string, 0)
	for _, m := range kclImport.FindAllStringSubmatch(source, -1) {
		path := m[1]

		// A leading dot makes an import explicitly relative, so it always
		// refers to a package inside this module.
		relative := strings.HasPrefix(path, ".")

		// Otherwise it is local only if the first segment names one of the
		// function's own subdirectories, rather than a registry dependency.
		head, _, _ := strings.Cut(strings.TrimLeft(path, "."), ".")
		dir, err := afero.DirExists(fnFS, head)
		if err != nil {
			return errors.Wrapf(err, "cannot check whether import %q is local", path)
		}

		if relative || dir {
			local = append(local, path)
		}
	}

	if len(local) == 0 {
		return nil
	}

	slices.Sort(local)

	return errors.Errorf("source imports %s from within the function, and only the function's top-level package is inlined; move the code into the top-level package or build this function as a package", strings.Join(slices.Compact(local), ", "))
}

// rewriteCompositions walks the Compositions staged for packaging and rewrites
// every pipeline step that references an inlined function. It returns the set
// of function refs it actually rewrote.
func rewriteCompositions(packageFS afero.Fs, inlined map[string]inlinedFunction) (map[string]bool, error) {
	used := make(map[string]bool, len(inlined))

	err := afero.Walk(packageFS, "/", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		bs, err := afero.ReadFile(packageFS, path)
		if err != nil {
			return errors.Wrapf(err, "cannot read %q", path)
		}

		var tm metav1.TypeMeta
		if err := yaml.Unmarshal(bs, &tm); err != nil {
			return errors.Wrapf(err, "cannot parse %q", path)
		}
		if tm.GroupVersionKind() != xpv1.CompositionGroupVersionKind {
			return nil
		}

		// Round-trip through a map rather than a typed Composition so fields
		// we don't know about survive the rewrite untouched.
		var comp map[string]any
		if err := yaml.Unmarshal(bs, &comp); err != nil {
			return errors.Wrapf(err, "cannot parse %q", path)
		}

		changed, err := rewritePipeline(comp, inlined, used)
		if err != nil {
			return errors.Wrapf(err, "cannot rewrite %q", path)
		}
		if !changed {
			return nil
		}

		out, err := yaml.Marshal(comp)
		if err != nil {
			return errors.Wrapf(err, "cannot marshal %q", path)
		}

		return errors.Wrapf(afero.WriteFile(packageFS, path, out, 0o644), "cannot write %q", path)
	})

	return used, err
}

// rewritePipeline replaces the functionRef of every step bound to an inlined
// function with the KCL runtime, and moves the function's source into the
// step's input. It reports whether it changed anything.
func rewritePipeline(comp map[string]any, inlined map[string]inlinedFunction, used map[string]bool) (bool, error) {
	spec, ok := comp["spec"].(map[string]any)
	if !ok {
		return false, nil
	}
	pipeline, ok := spec["pipeline"].([]any)
	if !ok {
		return false, nil
	}

	changed := false
	for i, s := range pipeline {
		step, ok := s.(map[string]any)
		if !ok {
			continue
		}
		ref, ok := step["functionRef"].(map[string]any)
		if !ok {
			continue
		}
		refName, _ := ref["name"].(string)
		fn, ok := inlined[refName]
		if !ok {
			continue
		}

		// The step's own input would be handed to the embedded function.
		// Inlining needs that field for the source, and silently dropping
		// whatever is there would change what the step does.
		if _, ok := step["input"]; ok {
			return false, errors.Errorf("step %d already sets input, which inlining needs for the function's source", i)
		}

		ref["name"] = functions.KCLRuntimeFunctionName

		inputSpec := map[string]any{"source": fn.source}
		if fn.dependencies != "" {
			inputSpec["dependencies"] = fn.dependencies
		}
		step["input"] = map[string]any{
			"apiVersion": kclInputAPIVersion,
			"kind":       kclInputKind,
			"spec":       inputSpec,
		}

		used[fn.ref] = true
		changed = true
	}

	return changed, nil
}
