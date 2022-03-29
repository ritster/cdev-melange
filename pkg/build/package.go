// Copyright 2022 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	apkofs "chainguard.dev/apko/pkg/fs"
	"chainguard.dev/apko/pkg/tarball"
	"chainguard.dev/melange/internal/sign"
	"github.com/psanford/memfs"
)

type PackageContext struct {
	Context       *Context
	Origin        *Package
	PackageName   string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Logger        *log.Logger
	Dependencies  Dependencies
}

func (pkg *Package) Emit(ctx *PipelineContext) error {
	fakesp := Subpackage{
		Name:         pkg.Name,
		Dependencies: pkg.Dependencies,
	}
	return fakesp.Emit(ctx)
}

func (spkg *Subpackage) Emit(ctx *PipelineContext) error {
	pc := PackageContext{
		Context:      ctx.Context,
		Origin:       &ctx.Context.Configuration.Package,
		PackageName:  spkg.Name,
		OutDir:       filepath.Join(ctx.Context.OutDir, ctx.Context.Arch.ToAPK()),
		Logger:       log.New(log.Writer(), fmt.Sprintf("melange (%s/%s): ", spkg.Name, ctx.Context.Arch.ToAPK()), log.LstdFlags|log.Lmsgprefix),
		Dependencies: spkg.Dependencies,
	}
	return pc.EmitPackage()
}

func (pc *PackageContext) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageContext) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageContext) WorkspaceSubdir() string {
	return filepath.Join(pc.Context.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `
# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = x86_64
size = {{.InstalledSize}}
pkgdesc = {{.Origin.Description}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Provides }}
provides = {{ $dep }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageContext) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageContext) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Context.SigningKey))
}

type DependencyGenerator func(*PackageContext, *Dependencies) error

func dedup(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))

	var prev string
	for _, cur := range in {
		if cur == prev {
			continue
		}
		out = append(out, cur)
		prev = cur
	}

	return out
}

func generateCmdProviders(pc *PackageContext, generated *Dependencies) error {
	pc.Logger.Printf("scanning for commands...")

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm() & 0555 == 0555 {
			if strings.Contains(path, "bin") {
				basename := filepath.Base(path)
				generated.Provides = append(generated.Provides, fmt.Sprintf("cmd:%s=%s-r%d", basename, pc.Origin.Version, pc.Origin.Epoch))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

func generateSharedObjectNameDeps(pc *PackageContext, generated *Dependencies) error {
	pc.Logger.Printf("scanning for shared object dependencies...")

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm() & 0555 == 0555 {
			basename := filepath.Base(path)
			if strings.Contains(basename, ".so.") {
				// TODO(kaniini): use strings.Cut when go1.18 is required
				parts := strings.Split(basename, ".so.")

				var libver string
				if len(parts) > 1 {
					libver = parts[1]
				} else {
					libver = "0"
				}

				generated.Provides = append(generated.Provides, fmt.Sprintf("so:%s=%s", basename, libver))
			}

			// most likely a shell script instead of an ELF, so treat any
			// error as non-fatal.
			// TODO(kaniini): use DirFS for this
			ef, err := elf.Open(filepath.Join(pc.WorkspaceSubdir(), path))
			if err != nil {
				return nil
			}
			defer ef.Close()

			libs, err := ef.ImportedLibraries()
			if err != nil {
				pc.Logger.Printf("WTF: ImportedLibraries() returned error: %v", err)
				return nil
			}

			for _, lib := range libs {
				generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", lib))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (dep *Dependencies) Summarize(logger *log.Logger) {
	if len(dep.Runtime) > 0 {
		logger.Printf("  runtime:")

		for _, dep := range dep.Runtime {
			logger.Printf("    %s", dep)
		}
	}

	if len(dep.Provides) > 0 {
		logger.Printf("  provides:")

		for _, dep := range dep.Provides {
			logger.Printf("    %s", dep)
		}
	}
}

func (pc *PackageContext) GenerateDependencies() error {
	generated := Dependencies{}
	generators := []DependencyGenerator{
		generateSharedObjectNameDeps,
		generateCmdProviders,
	}

	for _, gen := range generators {
		if err := gen(pc, &generated); err != nil {
			return err
		}
	}

	newruntime := append(pc.Dependencies.Runtime, generated.Runtime...)
	pc.Dependencies.Runtime = dedup(newruntime)

	newprovides := append(pc.Dependencies.Provides, generated.Provides...)
	pc.Dependencies.Provides = dedup(newprovides)

	pc.Dependencies.Summarize(pc.Logger)

	return nil
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageContext) EmitPackage() error {
	pc.Logger.Printf("generating package %s", pc.Identity())

	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()

	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	// generate so:/cmd: virtuals for the filesystem
	if err := pc.GenerateDependencies(); err != nil {
		return fmt.Errorf("unable to build final dependencies set: %w", err)
	}

	// prepare data.tar.gz
	dataDigest := sha256.New()
	dataMW := io.MultiWriter(dataDigest, dataTarGz)
	if err := tarctx.WriteArchive(dataMW, fsys); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	pc.DataHash = hex.EncodeToString(dataDigest.Sum(nil))
	pc.Logger.Printf("  data.tar.gz installed-size: %d", pc.InstalledSize)
	pc.Logger.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := dataTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	// prepare control.tar.gz
	multitarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return fmt.Errorf("unable to process control template: %w", err)
	}

	controlFS := memfs.New()
	if err := controlFS.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("unable to build control FS: %w", err)
	}

	controlTarGz, err := os.CreateTemp("", "melange-control-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer controlTarGz.Close()

	controlDigest := sha1.New() // nolint:gosec
	controlMW := io.MultiWriter(controlDigest, controlTarGz)
	if err := multitarctx.WriteArchive(controlMW, controlFS); err != nil {
		return fmt.Errorf("unable to write control tarball: %w", err)
	}

	controlHash := hex.EncodeToString(controlDigest.Sum(nil))
	pc.Logger.Printf("  control.tar.gz digest: %s", controlHash)

	if _, err := controlTarGz.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind control tarball: %w", err)
	}

	combinedParts := []io.Reader{controlTarGz, dataTarGz}

	if pc.Context.SigningKey != "" {
		signatureFS := memfs.New()
		signatureBuf, err := sign.RSASignSHA1Digest(controlDigest.Sum(nil),
			pc.Context.SigningKey, pc.Context.SigningPassphrase)
		if err != nil {
			return fmt.Errorf("unable to generate signature: %w", err)
		}

		if err := signatureFS.WriteFile(pc.SignatureName(), signatureBuf, 0644); err != nil {
			return fmt.Errorf("unable to build signature FS: %w", err)
		}

		signatureTarGz, err := os.CreateTemp("", "melange-signature-*.tar.gz")
		if err != nil {
			return fmt.Errorf("unable to open temporary file for writing: %w", err)
		}
		defer signatureTarGz.Close()

		if err := multitarctx.WriteArchive(signatureTarGz, signatureFS); err != nil {
			return fmt.Errorf("unable to write signature tarball: %w", err)
		}

		if _, err := signatureTarGz.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("unable to rewind signature tarball: %w", err)
		}

		combinedParts = append([]io.Reader{signatureTarGz}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	pc.Logger.Printf("wrote %s", outFile.Name())

	return nil
}