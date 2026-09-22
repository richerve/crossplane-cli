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
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/afero"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xpkg"

	xpv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	opsv1alpha1 "github.com/crossplane/crossplane/apis/v2/ops/v1alpha1"

	devv1alpha1 "github.com/crossplane/cli/v2/apis/dev/v1alpha1"
	"github.com/crossplane/cli/v2/internal/filesystem"
)

const (
	// functionVersionLength is how many hex characters of a function's source
	// hash are appended to its repository path. Twelve characters is 48 bits,
	// which is far more than enough to keep the versions of one function
	// distinct, and short enough to leave the repository path readable.
	functionVersionLength = 12

	// pythonProjectFile marks a function directory as a Python function. Only
	// the Python builder stages the project's generated schemas alongside the
	// function source, so only Python functions need those schemas folded into
	// their source hash.
	pythonProjectFile = "pyproject.toml"

	// pythonSchemasDir is the schemas subdirectory the Python builder stages.
	pythonSchemasDir = "python"

	// pythonVenvDir is a virtualenv a developer may have created in a function
	// directory. The Python builder excludes it from the image, so it must be
	// excluded from the hash too, or a function would version differently
	// depending on whether anyone had worked on it locally.
	pythonVenvDir = ".venv"
)

// functionVersion is the naming one embedded function takes in a versioned
// build.
type functionVersion struct {
	// repo is the OCI repository the function's package is pushed to, with the
	// source hash appended.
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

// versionFunctions computes the versioned naming for each of a project's
// embedded functions, keyed by function name.
//
// It must run after schema generation: a KCL or Go function reaches the
// project's generated models through a symlink in its own directory, so the
// models are part of the source being hashed.
func versionFunctions(projectFS afero.Fs, project *devv1alpha1.Project, fns []devv1alpha1.Function, basePath string) (map[string]functionVersion, error) {
	versions := make(map[string]functionVersion, len(fns))
	for _, fn := range fns {
		fnName := fn.Name()

		version, err := hashFunctionSource(projectFS, project, fn, basePath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to hash source of function %q", fnName)
		}

		stableRef, err := functionRef(project.Spec.Repository, fnName, "")
		if err != nil {
			return nil, err
		}
		ref, err := functionRef(project.Spec.Repository, fnName, version)
		if err != nil {
			return nil, err
		}

		// A functionRef is a DNS label, so it is cut at 63 characters. A
		// repository path long enough to push the version past the cut would
		// give every version of the function the same name, which is both the
		// thing versioning exists to prevent and invisible once built.
		if !strings.HasSuffix(ref, "-"+version) {
			return nil, errors.Errorf("repository %q is too long to version function %q: the function reference is a DNS label, so the version is cut off it, leaving every version of the function named %q; shorten spec.repository or the function name", project.Spec.Repository, fnName, ref)
		}

		versions[fnName] = functionVersion{
			repo:      functionRepository(project.Spec.Repository, fnName, version),
			stableRef: stableRef,
			ref:       ref,
		}
	}

	return versions, nil
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

// hashFunctionSource returns a hex digest of everything that goes into a
// function's runtime image, truncated to functionVersionLength.
//
// It deliberately hashes the source rather than the built image. Image digests
// are not reproducible - layers carry file modification times, and Go and
// Python functions ship compiled artefacts - so a digest-derived name would
// mint a new repository, a new Function object and a new running pod on every
// build, including builds that changed nothing.
func hashFunctionSource(projectFS afero.Fs, project *devv1alpha1.Project, fn devv1alpha1.Function, basePath string) (string, error) {
	var parts []string

	switch fn.Source {
	case devv1alpha1.FunctionSourceDirectory:
		var err error
		if parts, err = directorySourceEntries(projectFS, project, fn, basePath); err != nil {
			return "", err
		}
	case devv1alpha1.FunctionSourceTarball:
		var err error
		if parts, err = tarballSourceEntries(projectFS, project, fn); err != nil {
			return "", err
		}
	default:
		// Should be caught at validation time, but be defensive.
		return "", errors.Errorf("unsupported function source %q", fn.Source)
	}

	// The entries are sorted so that the hash does not depend on the order the
	// filesystem happened to enumerate them in, which is not guaranteed to be
	// stable across machines.
	sort.Strings(parts)

	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = io.WriteString(h, "\n")
	}

	return hex.EncodeToString(h.Sum(nil))[:functionVersionLength], nil
}

// directorySourceEntries returns a hashable entry per file that a
// Directory-source function contributes to its runtime image.
//
// It reuses FSToTar, the same call the language builders make, so that what is
// hashed is what is built: symlinks are followed into the project's generated
// models, and the directories the builders skip are skipped here too.
func directorySourceEntries(projectFS afero.Fs, project *devv1alpha1.Project, fn devv1alpha1.Function, basePath string) ([]string, error) {
	fnDir := filepath.Join(project.Spec.Paths.Functions, fn.Directory.Name)
	fnFS := afero.NewBasePathFs(projectFS, fnDir)

	src, err := filesystem.FSToTar(fnFS, "/",
		filesystem.WithSymlinkBasePath(functionBasePath(fnFS, project, fn.Directory.Name, basePath)),
		filesystem.WithExcludePrefix(pythonVenvDir),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to tar function source")
	}

	entries, err := tarEntries(src)
	if err != nil {
		return nil, err
	}

	// A Python function's generated schemas live outside its directory - the
	// builder stages them from the project's schemas path - so nothing above
	// has seen them.
	isPython, err := afero.Exists(fnFS, pythonProjectFile)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot check for %s", pythonProjectFile)
	}
	if !isPython {
		return entries, nil
	}

	schemasRel := path.Join(project.Spec.Paths.Schemas, pythonSchemasDir)
	schemasFS := afero.NewBasePathFs(projectFS, schemasRel)
	hasSchemas, err := afero.DirExists(schemasFS, ".")
	if err != nil {
		return nil, errors.Wrapf(err, "cannot check for python schemas at %q", schemasRel)
	}
	if !hasSchemas {
		return entries, nil
	}

	schemas, err := filesystem.FSToTar(schemasFS, schemasRel)
	if err != nil {
		return nil, errors.Wrap(err, "failed to tar python schemas")
	}

	schemaEntries, err := tarEntries(schemas)
	if err != nil {
		return nil, err
	}

	return append(entries, schemaEntries...), nil
}

// tarballSourceEntries returns a hashable entry per runtime tarball a
// Tarball-source function is built from.
func tarballSourceEntries(projectFS afero.Fs, project *devv1alpha1.Project, fn devv1alpha1.Function) ([]string, error) {
	entries := make([]string, 0, len(project.Spec.Architectures))
	for _, arch := range project.Spec.Architectures {
		found := false
		for _, ext := range []string{".tar", ".tar.gz"} {
			p := fmt.Sprintf("%s-%s%s", fn.Tarball.PathPrefix, arch, ext)
			exists, err := afero.Exists(projectFS, p)
			if err != nil {
				return nil, errors.Wrapf(err, "cannot check for runtime tarball %q", p)
			}
			if !exists {
				continue
			}

			sum, err := hashFile(projectFS, p)
			if err != nil {
				return nil, err
			}
			entries = append(entries, fmt.Sprintf("%s %s", arch, sum))
			found = true

			break
		}
		if !found {
			return nil, errors.Errorf("no runtime tarball found for architecture %q at %q", arch, fn.Tarball.PathPrefix)
		}
	}

	return entries, nil
}

func hashFile(f afero.Fs, path string) (string, error) {
	file, err := f.Open(path)
	if err != nil {
		return "", errors.Wrapf(err, "cannot open %q", path)
	}
	defer file.Close() //nolint:errcheck // Read-only.

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", errors.Wrapf(err, "cannot read %q", path)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// tarEntries reduces a tar archive to one string per regular file, holding the
// file's path, mode and a digest of its contents.
//
// Modification times, ownership and the order entries appear in are all left
// out: none of them change what the function does, and all of them differ
// between two checkouts of the same commit.
func tarEntries(src []byte) ([]string, error) {
	entries := make([]string, 0)
	r := tar.NewReader(bytes.NewReader(src))
	for {
		hdr, err := r.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, errors.Wrap(err, "cannot read function source archive")
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		h := sha256.New()
		if _, err := io.Copy(h, r); err != nil { //nolint:gosec // Reading an archive we just produced.
			return nil, errors.Wrapf(err, "cannot read %q from function source archive", hdr.Name)
		}

		entries = append(entries, fmt.Sprintf("%s %04o %s", hdr.Name, hdr.Mode&0o777, hex.EncodeToString(h.Sum(nil))))
	}
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
