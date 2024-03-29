// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package task

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/build/buildenv"
	"golang.org/x/build/buildlet"
	"golang.org/x/build/dashboard"
	"golang.org/x/build/internal/releasetargets"
	"golang.org/x/build/internal/workflow"
)

// WriteSourceArchive writes a source archive to out, based on revision with version written in as VERSION.
func WriteSourceArchive(ctx *workflow.TaskContext, source io.Reader, version string, out io.Writer) error {
	ctx.Printf("Create source archive.")
	gzReader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	reader := tar.NewReader(gzReader)

	gzWriter := gzip.NewWriter(out)
	writer := tar.NewWriter(gzWriter)

	// Add go/VERSION to the archive, and fix up the existing contents.
	if err := writer.WriteHeader(&tar.Header{
		Name:       "go/VERSION",
		Size:       int64(len(version)),
		Typeflag:   tar.TypeReg,
		Mode:       0644,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := writer.Write([]byte(version)); err != nil {
		return err
	}
	if err := adjustTar(reader, writer, "go/", []adjustFunc{
		dropRegexpMatches([]string{`VERSION`}), // Don't overwrite our VERSION file from above.
		dropRegexpMatches(dropPatterns),
		fixPermissions(),
	}); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}
	return gzWriter.Close()
}

// An adjustFunc updates a tar file header in some way.
// The input is safe to write to. A nil return means to drop the file.
type adjustFunc func(*tar.Header) *tar.Header

// adjustTar copies the files from reader to writer, putting them in prefixDir
// and adjusting them with adjusts along the way. Prefix must have a trailing /.
func adjustTar(reader *tar.Reader, writer *tar.Writer, prefixDir string, adjusts []adjustFunc) error {
	if !strings.HasSuffix(prefixDir, "/") {
		return fmt.Errorf("prefix dir %q must have a trailing /", prefixDir)
	}
	writer.WriteHeader(&tar.Header{
		Name:       prefixDir,
		Typeflag:   tar.TypeDir,
		Mode:       0755,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	})
file:
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		headerCopy := *header
		newHeader := &headerCopy
		for _, adjust := range adjusts {
			newHeader = adjust(newHeader)
			if newHeader == nil {
				continue file
			}
		}
		newHeader.Name = prefixDir + newHeader.Name
		writer.WriteHeader(newHeader)
		if _, err := io.Copy(writer, reader); err != nil {
			return err
		}
	}
	return nil
}

var dropPatterns = []string{
	// .gitattributes, .github, etc.
	`\..*`,
	// This shouldn't exist, since we create a VERSION file.
	`VERSION.cache`,
	// Remove the build cache that the toolchain build process creates.
	// According to go.dev/cl/82095, it shouldn't exist at all.
	`pkg/obj/.*`,
	// Users don't need the api checker binary pre-built. It's
	// used by tests, but all.bash builds it first.
	`pkg/tool/[^/]+/api.*`,
	// Users also don't need the metadata command, which is run dynamically
	// by cmd/dist. As of writing we don't know why it's showing up at all.
	`pkg/tool/[^/]+/metadata.*`,
	// Remove pkg/${GOOS}_${GOARCH}/cmd. This saves a bunch of
	// space, and users don't typically rebuild cmd/compile,
	// cmd/link, etc. If they want to, they still can, but they'll
	// have to pay the cost of rebuilding dependent libraries. No
	// need to ship them just in case.
	`pkg/[^/]+/cmd/.*`,
	// Clean up .exe~ files; see go.dev/issue/23894.
	`.*\.exe~`,
}

// dropRegexpMatches drops files whose name matches any of patterns.
func dropRegexpMatches(patterns []string) adjustFunc {
	var rejectRegexps []*regexp.Regexp
	for _, pattern := range patterns {
		rejectRegexps = append(rejectRegexps, regexp.MustCompile("^"+pattern+"$"))
	}
	return func(h *tar.Header) *tar.Header {
		for _, regexp := range rejectRegexps {
			if regexp.MatchString(h.Name) {
				return nil
			}
		}
		return h
	}
}

// dropUnwantedSysos drops race detector sysos for other architectures.
func dropUnwantedSysos(target *releasetargets.Target) adjustFunc {
	raceSysoRegexp := regexp.MustCompile(`^src/runtime/race/race_(.*?).syso$`)
	osarch := target.GOOS + "_" + target.GOARCH
	return func(h *tar.Header) *tar.Header {
		matches := raceSysoRegexp.FindStringSubmatch(h.Name)
		if matches != nil && matches[1] != osarch {
			return nil
		}
		return h
	}
}

// fixPermissions sets files' permissions to user-writeable, world-readable.
func fixPermissions() adjustFunc {
	return func(h *tar.Header) *tar.Header {
		if h.Typeflag == tar.TypeDir || h.Mode&0111 != 0 {
			h.Mode = 0755
		} else {
			h.Mode = 0644
		}
		return h
	}
}

// fixupCrossCompile moves cross-compiled tools to their final location and
// drops unnecessary host architecture files.
func fixupCrossCompile(target *releasetargets.Target) adjustFunc {
	if !strings.HasSuffix(target.Builder, "-crosscompile") {
		return func(h *tar.Header) *tar.Header { return h }
	}
	osarch := target.GOOS + "_" + target.GOARCH
	return func(h *tar.Header) *tar.Header {
		// Move cross-compiled tools up to bin/, and drop the existing contents.
		if strings.HasPrefix(h.Name, "bin/") {
			if strings.HasPrefix(h.Name, "bin/"+osarch) {
				h.Name = strings.ReplaceAll(h.Name, "bin/"+osarch, "bin")
			} else {
				return nil
			}
		}
		// Drop host architecture files.
		if strings.HasPrefix(h.Name, "pkg/linux_amd64") ||
			strings.HasPrefix(h.Name, "pkg/tool/linux_amd64") {
			return nil
		}
		return h
	}
}

const (
	goDir = "go"
	go14  = "go1.4"
)

type BuildletStep struct {
	Target      *releasetargets.Target
	Buildlet    buildlet.RemoteClient
	BuildConfig *dashboard.BuildConfig
	LogWriter   io.Writer
}

// BuildBinary builds a binary distribution from sourceArchive and writes it to out.
func (b *BuildletStep) BuildBinary(ctx *workflow.TaskContext, sourceArchive io.Reader, out io.Writer) error {
	// Push source to buildlet.
	ctx.Printf("Pushing source to buildlet.")
	if err := b.Buildlet.PutTar(ctx, sourceArchive, ""); err != nil {
		return fmt.Errorf("failed to put generated source tarball: %v", err)
	}

	if u := b.BuildConfig.GoBootstrapURL(buildenv.Production); u != "" {
		ctx.Printf("Installing go1.4.")
		if err := b.Buildlet.PutTarFromURL(ctx, u, go14); err != nil {
			return err
		}
	}

	// Execute build (make.bash only first).
	ctx.Printf("Building (make.bash only).")
	if err := b.exec(ctx, goDir+"/"+b.BuildConfig.MakeScript(), b.BuildConfig.MakeScriptArgs(), buildlet.ExecOpts{
		ExtraEnv: b.makeEnv(),
	}); err != nil {
		return err
	}

	ctx.Printf("Building release tarball.")
	input, err := b.Buildlet.GetTar(ctx, "go")
	if err != nil {
		return err
	}
	defer input.Close()

	gzReader, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer gzReader.Close()
	reader := tar.NewReader(gzReader)
	gzWriter := gzip.NewWriter(out)
	writer := tar.NewWriter(gzWriter)
	if err := adjustTar(reader, writer, "go/", []adjustFunc{
		dropRegexpMatches(dropPatterns),
		dropUnwantedSysos(b.Target),
		fixupCrossCompile(b.Target),
		fixPermissions(),
	}); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return gzWriter.Close()
}

func (b *BuildletStep) makeEnv() []string {
	// We need GOROOT_FINAL both during the binary build and test runs. See go.dev/issue/52236.
	makeEnv := []string{"GOROOT_FINAL=" + dashboard.GorootFinal(b.Target.GOOS)}
	// Add extra vars from the target's configuration.
	makeEnv = append(makeEnv, b.Target.ExtraEnv...)
	return makeEnv
}

// ExtractFile copies the first file in tgz matching glob to dest.
func ExtractFile(tgz io.Reader, dest io.Writer, glob string) error {
	zr, err := gzip.NewReader(tgz)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		} else if err != nil {
			return err
		}
		if match, _ := path.Match(glob, h.Name); !h.FileInfo().IsDir() && match {
			break
		}
	}
	_, err = io.Copy(dest, tr)
	return err
}

func (b *BuildletStep) TestTarget(ctx *workflow.TaskContext, binaryArchive io.Reader) error {
	if err := b.Buildlet.PutTar(ctx, binaryArchive, ""); err != nil {
		return err
	}
	if u := b.BuildConfig.GoBootstrapURL(buildenv.Production); u != "" {
		ctx.Printf("Installing go1.4 (second time, for all.bash).")
		if err := b.Buildlet.PutTarFromURL(ctx, u, go14); err != nil {
			return err
		}
	}
	ctx.Printf("Building (all.bash to ensure tests pass).")
	ctx.SetWatchdogScale(b.BuildConfig.GoTestTimeoutScale())
	return b.exec(ctx, goDir+"/"+b.BuildConfig.AllScript(), b.BuildConfig.AllScriptArgs(), buildlet.ExecOpts{
		ExtraEnv: b.makeEnv(),
	})
}

func (b *BuildletStep) RunTryBot(ctx *workflow.TaskContext, sourceArchive io.Reader) error {
	ctx.Printf("Pushing source to buildlet.")
	if err := b.Buildlet.PutTar(ctx, sourceArchive, ""); err != nil {
		return fmt.Errorf("failed to put generated source tarball: %v", err)
	}

	if u := b.BuildConfig.GoBootstrapURL(buildenv.Production); u != "" {
		ctx.Printf("Installing go1.4.")
		if err := b.Buildlet.PutTarFromURL(ctx, u, go14); err != nil {
			return err
		}
	}

	if b.BuildConfig.CompileOnly {
		ctx.Printf("Building toolchain (make.bash).")
		if err := b.exec(ctx, goDir+"/"+b.BuildConfig.MakeScript(), b.BuildConfig.MakeScriptArgs(), buildlet.ExecOpts{}); err != nil {
			return fmt.Errorf("building toolchain failed: %v", err)
		}
		ctx.Printf("Compiling-only tests (via BuildConfig.CompileOnly).")
		if err := b.runGo(ctx, []string{"tool", "dist", "test", "-compile-only"}, buildlet.ExecOpts{}); err != nil {
			return fmt.Errorf("building tests failed: %v", err)
		}
		return nil
	}

	ctx.Printf("Testing (all.bash).")
	ctx.SetWatchdogScale(b.BuildConfig.GoTestTimeoutScale())
	err := b.exec(ctx, goDir+"/"+b.BuildConfig.AllScript(), b.BuildConfig.AllScriptArgs(), buildlet.ExecOpts{})
	if err != nil {
		ctx.Printf("testing failed: %v", err)
	}
	return err
}

// exec runs cmd with args. Its working dir is opts.Dir, or the directory of cmd.
// Its environment is the buildlet's environment, plus a GOPATH setting, plus opts.ExtraEnv.
// If the command fails, its output is included in the returned error.
func (b *BuildletStep) exec(ctx context.Context, cmd string, args []string, opts buildlet.ExecOpts) error {
	work, err := b.Buildlet.WorkDir(ctx)
	if err != nil {
		return err
	}

	// Set up build environment. The caller's environment wins if there's a conflict.
	env := append(b.BuildConfig.Env(), "GOPATH="+work+"/gopath")
	env = append(env, opts.ExtraEnv...)
	opts.Output = b.LogWriter
	opts.ExtraEnv = env
	opts.Args = args
	opts.Debug = true // Print debug info.
	remoteErr, execErr := b.Buildlet.Exec(ctx, cmd, opts)
	if execErr != nil {
		return execErr
	}
	if remoteErr != nil {
		return fmt.Errorf("Command %v %s failed: %v", cmd, args, remoteErr)
	}

	return nil
}

// runGo runs the go command with args using BuildletStep.exec.
func (b *BuildletStep) runGo(ctx context.Context, args []string, execOpts buildlet.ExecOpts) error {
	return b.exec(ctx, goDir+"/bin/go", args, execOpts)
}

func ConvertTGZToZIP(r io.Reader, w io.Writer) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)

	zw := zip.NewWriter(w)
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		fi := th.FileInfo()
		zh, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		zh.Name = th.Name // for the full path
		switch strings.ToLower(path.Ext(zh.Name)) {
		case ".jpg", ".jpeg", ".png", ".gif":
			// Don't re-compress already compressed files.
			zh.Method = zip.Store
		default:
			zh.Method = zip.Deflate
		}
		if fi.IsDir() {
			zh.Method = zip.Store
		}
		w, err := zw.CreateHeader(zh)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			continue
		}
		if _, err := io.Copy(w, tr); err != nil {
			return err
		}
	}
	return zw.Close()
}

func ConvertZIPToTGZ(r io.ReaderAt, size int64, w io.Writer) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}

	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)

	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     f.Name,
			Typeflag: tar.TypeReg,
			Mode:     0o777,
			Size:     int64(f.UncompressedSize64),

			AccessTime: f.Modified,
			ChangeTime: f.Modified,
			ModTime:    f.Modified,
		}); err != nil {
			return err
		}
		content, err := f.Open()
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, content); err != nil {
			return err
		}
		if err := content.Close(); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}
