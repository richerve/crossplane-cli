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
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xpkg"

	xpv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	opsv1alpha1 "github.com/crossplane/crossplane/apis/v2/ops/v1alpha1"
)

// functionVersionLength is how many hex characters of a function's index
// digest are appended to its repository path. Twelve characters is 48 bits,
// which is far more than enough to keep the versions of one function distinct,
// and short enough to leave the repository path readable.
const functionVersionLength = 12

// functionVersion is the naming one embedded function takes in a versioned
// build.
type functionVersion struct {
	// repo is the OCI repository the function's package is pushed to, with the
	// start of its index digest appended.
	repo string

	// stableRef is the functionRef name pipeline steps carry in the project's
	// source. `crossplane function generate` derives it from the unversioned
	// repository path, so it is what a rewrite has to match on.
	stableRef string

	// ref is the functionRef name steps are rewritten to. Crossplane names a
	// package it installs for a dependency after that package's repository
	// path, so this is the name the versioned Function object will have.
	ref string
}

// versionFunction returns the versioned naming for an embedded function whose
// package index has digest dgst.
//
// The version comes from what was built rather than from the source, so that a
// function's name changes exactly when the package a Configuration depends on
// changes. Crossplane will not move an installed dependency to a different
// digest, so a name that stayed put while its digest moved - an unchanged
// source rebuilt onto a patched base image, or resolving a newer Python
// dependency - would leave the Configuration unable to upgrade. Builds are
// reproducible, so rebuilding an unchanged function gives the same digest and
// keeps its name.
func versionFunction(repository, fnName string, dgst v1.Hash) (functionVersion, error) {
	if len(dgst.Hex) < functionVersionLength {
		return functionVersion{}, errors.Errorf("cannot version function %q: digest %q is too short", fnName, dgst)
	}
	version := dgst.Hex[:functionVersionLength]

	stableRef, err := functionRef(repository, fnName, "")
	if err != nil {
		return functionVersion{}, err
	}
	ref, err := functionRef(repository, fnName, version)
	if err != nil {
		return functionVersion{}, err
	}

	// A functionRef is a DNS label, so it is cut at 63 characters. A
	// repository path long enough to push the version past the cut would give
	// every version of the function the same name, which is both the thing
	// versioning exists to prevent and invisible once built.
	if !strings.HasSuffix(ref, "-"+version) {
		return functionVersion{}, errors.Errorf("repository %q is too long to version function %q: the function reference is a DNS label, so the version is cut off it, leaving every version of the function named %q; shorten spec.repository or the function name", repository, fnName, ref)
	}

	return functionVersion{
		repo:      functionRepository(repository, fnName, version),
		stableRef: stableRef,
		ref:       ref,
	}, nil
}

// functionRepository returns the OCI repository path for a function. An empty
// version yields the unversioned path.
func functionRepository(repository, fnName, version string) string {
	repo := fmt.Sprintf("%s_%s", repository, fnName)
	if version == "" {
		return repo
	}

	return fmt.Sprintf("%s-%s", repo, version)
}

// functionRef returns the functionRef name a pipeline step uses to reach a
// function, which Crossplane derives from the function package's repository
// path.
func functionRef(repository, fnName, version string) (string, error) {
	repo, err := name.NewRepository(functionRepository(repository, fnName, version))
	if err != nil {
		return "", errors.Wrapf(err, "cannot build function reference from repository %q", repository)
	}

	return xpkg.ToDNSLabel(repo.RepositoryStr()), nil
}

// applyFunctionVersions rewrites the staged resources to call the versions that
// were just built, and reports any function nothing in this project calls.
//
// An uncalled function is not an error: a project may exist to publish
// functions other projects compose with. Those projects have to name the
// version they want, though, so it is worth saying out loud.
func applyFunctionVersions(packageFS afero.Fs, versions map[string]functionVersion, log logging.Logger) error {
	if len(versions) == 0 {
		return nil
	}

	used, err := rewriteFunctionRefs(packageFS, versions)
	if err != nil {
		return err
	}

	for fnName, v := range versions {
		if !used[fnName] {
			log.Info("Function is versioned but no pipeline step in this project references it",
				"function", fnName, "function-ref", v.stableRef)
		}
	}

	return nil
}

// rewriteFunctionRefs repoints every pipeline step in packageFS that names an
// embedded function at that function's versioned name. The resources staged
// for packaging are modified in place; the project's own files are never
// touched.
//
// It returns the set of function names it rewrote at least one step for.
func rewriteFunctionRefs(packageFS afero.Fs, versions map[string]functionVersion) (map[string]bool, error) {
	// Steps name functions by ref, so index the other way around.
	byRef := make(map[string]functionVersion, len(versions))
	names := make(map[string]string, len(versions))
	for fnName, v := range versions {
		byRef[v.stableRef] = v
		names[v.stableRef] = fnName
	}

	used := make(map[string]bool, len(versions))

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

		// Round-trip through a map rather than a typed object so fields we
		// don't know about survive the rewrite untouched.
		var obj map[string]any
		if err := yaml.Unmarshal(bs, &obj); err != nil {
			return errors.Wrapf(err, "cannot parse %q", path)
		}

		changed := false
		for _, s := range pipelineOf(tm, obj) {
			step, ok := s.(map[string]any)
			if !ok {
				continue
			}
			ref, ok := step["functionRef"].(map[string]any)
			if !ok {
				continue
			}
			refName, _ := ref["name"].(string)
			v, ok := byRef[refName]
			if !ok {
				continue
			}

			ref["name"] = v.ref
			used[names[refName]] = true
			changed = true
		}
		if !changed {
			return nil
		}

		out, err := yaml.Marshal(obj)
		if err != nil {
			return errors.Wrapf(err, "cannot marshal %q", path)
		}

		return errors.Wrapf(afero.WriteFile(packageFS, path, out, 0o644), "cannot write %q", path)
	})

	return used, err
}

// pipelineOf returns the function pipeline of a staged resource, or nil if it
// has none. A Composition and an Operation hold theirs directly; a
// CronOperation and a WatchOperation hold theirs in the Operation they
// template.
func pipelineOf(tm metav1.TypeMeta, obj map[string]any) []any {
	var specPath []string
	switch tm.GroupVersionKind() {
	case xpv1.CompositionGroupVersionKind, opsv1alpha1.OperationGroupVersionKind:
		specPath = []string{"spec"}
	case opsv1alpha1.CronOperationGroupVersionKind, opsv1alpha1.WatchOperationGroupVersionKind:
		specPath = []string{"spec", "operationTemplate", "spec"}
	default:
		return nil
	}

	cur := obj
	for _, k := range specPath {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}

	pipeline, _ := cur["pipeline"].([]any)

	return pipeline
}
