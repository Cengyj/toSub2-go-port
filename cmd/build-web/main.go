// Command build-web bundles the browser console with the Go esbuild API.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "verify both generated trees against current sources without replacing them")
	buildGo := flag.Bool("build-go", false, "build the Go program while holding the workspace build lock")
	cross := flag.Bool("cross", false, "build all supported release targets (requires -build-go)")
	buildDir := flag.String("build-dir", os.Getenv("TOSUB2_BUILD_DIR"), "program output directory (defaults to TOSUB2_BUILD_DIR or bin)")
	flag.Parse()
	var err error
	if !*buildGo && *cross {
		err = errors.New("-cross requires -build-go")
	}
	if err == nil {
		if *buildGo {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = buildWebProgram(ctx, *rootFlag, *check, programBuild{directory: *buildDir, cross: *cross})
		} else {
			err = buildWebMode(*rootFlag, *check)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func buildWeb(root string) error {
	return buildWebMode(root, false)
}

func buildWebMode(root string, check bool) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	release, err := acquireBuildLock(root)
	if err != nil {
		return err
	}
	defer release()
	return buildWebLocked(root, check)
}

// The caller holds the workspace lock through every consumer of these trees.
func buildWebLocked(root string, check bool) error {
	web := filepath.Join(root, "web")
	src := filepath.Join(web, "src", "main.jsx")
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("web source: %w", err)
	}
	index, err := os.ReadFile(filepath.Join(web, "index.html"))
	if err != nil {
		return fmt.Errorf("HTML template: %w", err)
	}
	if strings.Count(string(index), "/src/main.jsx") != 1 || !strings.Contains(string(index), "</head>") {
		return fmt.Errorf("HTML template must contain one /src/main.jsx script and a closing head")
	}
	tmp, err := os.MkdirTemp(web, ".dist-build-")
	if err != nil {
		return fmt.Errorf("temporary output: %w", err)
	}
	defer os.RemoveAll(tmp)
	vendor := filepath.Join(web, "vendor")
	aliases := map[string]string{
		"react":            filepath.Join(vendor, "react", "react.production.js"),
		"react-dom":        filepath.Join(vendor, "react-dom", "react-dom.production.js"),
		"react-dom/client": filepath.Join(vendor, "react-dom", "react-dom-client.production.js"),
		"scheduler":        filepath.Join(vendor, "scheduler", "scheduler.production.js"),
		"lucide-react":     filepath.Join(vendor, "lucide-react", "index.js"),
	}
	result := api.Build(api.BuildOptions{
		AbsWorkingDir:     web,
		EntryPoints:       []string{src},
		Outfile:           filepath.Join(tmp, "assets", "index.js"),
		Bundle:            true,
		Platform:          api.PlatformBrowser,
		Format:            api.FormatESModule,
		Target:            api.ES2020,
		MinifySyntax:      true,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		Define:            map[string]string{"process.env.NODE_ENV": "\"production\""},
		Loader:            map[string]api.Loader{".js": api.LoaderJS, ".jsx": api.LoaderJSX, ".css": api.LoaderCSS},
		Plugins: []api.Plugin{{Name: "browser-sources-only", Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `^[^./]`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if target, ok := aliases[args.Path]; ok {
					return api.OnResolveResult{Path: target}, nil
				}
				// No package-manager discovery or ambient node_modules fallback.
				if args.Kind == api.ResolveEntryPoint {
					return api.OnResolveResult{}, nil
				}
				return api.OnResolveResult{}, fmt.Errorf("unvendored browser import %q", args.Path)
			})
		}}},
		Write:    true,
		LogLevel: api.LogLevelWarning,
	})
	if len(result.Errors) > 0 {
		return fmt.Errorf("esbuild failed")
	}
	jsName, err := hashAsset(tmp, "index.js")
	if err != nil {
		return err
	}
	cssName, err := hashAsset(tmp, "index.css")
	if err != nil {
		return err
	}
	html := strings.Replace(string(index), "/src/main.jsx", "/assets/"+jsName, 1)
	html = strings.Replace(html, "</head>", "  <link rel=\"stylesheet\" href=\"/assets/"+cssName+"\" />\n  </head>", 1)
	if err := os.WriteFile(filepath.Join(tmp, "index.html"), []byte(html), 0644); err != nil {
		return err
	}
	var notices strings.Builder
	for _, name := range []string{"react", "react-dom", "scheduler", "lucide-react"} {
		license, err := os.ReadFile(filepath.Join(vendor, name, "LICENSE"))
		if err != nil {
			return err
		}
		fmt.Fprintf(&notices, "%s\n%s\n\n", name, license)
	}
	if err := os.WriteFile(filepath.Join(tmp, "THIRD_PARTY_NOTICES.txt"), []byte(strings.TrimRight(notices.String(), "\r\n")+"\n"), 0644); err != nil {
		return err
	}
	out := filepath.Join(web, "dist")
	embedded := filepath.Join(root, "cmd", "tosub2", "web", "dist")
	if check {
		return errors.Join(compareTree(tmp, out), compareTree(tmp, embedded))
	}
	if err := installOutputs(tmp, []string{out, embedded}, os.Rename); err != nil {
		return err
	}
	fmt.Printf("built %s and %s\n", out, embedded)
	return nil
}

// Prepare every destination before replacing either tree. Rename failures roll
// back earlier replacements; backups survive a failed rollback for inspection.
// Two directories cannot be committed atomically across a process crash. The
// read-only -check gate detects that case before packaging existing assets.
func installOutputs(source string, targets []string, rename func(string, string) error) (result error) {
	type output struct {
		target, staging     string
		backedUp, installed bool
	}
	outputs := make([]output, 0, len(targets))
	defer func() {
		for _, out := range outputs {
			if !out.backedUp {
				result = errors.Join(result, os.RemoveAll(out.staging))
			}
		}
	}()
	for _, target := range targets {
		if info, err := os.Lstat(target); err == nil && !info.IsDir() {
			return fmt.Errorf("generated output must be a directory: %s", target)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		staging, err := os.MkdirTemp(filepath.Dir(target), ".dist-install-")
		if err != nil {
			return err
		}
		outputs = append(outputs, output{target: target, staging: staging})
		if err := copyTree(source, filepath.Join(staging, "next")); err != nil {
			return fmt.Errorf("prepare %s: %w", target, err)
		}
	}
	rollback := func(cause error) error {
		for i := len(outputs) - 1; i >= 0; i-- {
			out := &outputs[i]
			if out.installed {
				if err := os.RemoveAll(out.target); err != nil {
					cause = errors.Join(cause, fmt.Errorf("remove failed replacement %s: %w", out.target, err))
					continue
				}
			}
			if out.backedUp {
				if err := rename(filepath.Join(out.staging, "previous"), out.target); err != nil {
					cause = errors.Join(cause, fmt.Errorf("restore %s from %s: %w", out.target, out.staging, err))
				} else {
					out.backedUp = false
				}
			}
		}
		return cause
	}
	for i := range outputs {
		out := &outputs[i]
		if err := rename(out.target, filepath.Join(out.staging, "previous")); err == nil {
			out.backedUp = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollback(fmt.Errorf("preserve %s: %w", out.target, err))
		}
		if err := rename(filepath.Join(out.staging, "next"), out.target); err != nil {
			return rollback(fmt.Errorf("install %s: %w", out.target, err))
		}
		out.installed = true
	}
	for i := range outputs {
		outputs[i].backedUp = false
	}
	return nil
}

func compareTree(expected, actual string) error {
	read := func(root string) (map[string][]byte, error) {
		files := map[string][]byte{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("unexpected generated file type: %s", path)
			}
			name, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files[name], err = os.ReadFile(path)
			return err
		})
		return files, err
	}
	want, err := read(expected)
	if err != nil {
		return err
	}
	got, err := read(actual)
	if err != nil {
		return fmt.Errorf("check %s: %w", actual, err)
	}
	if len(want) != len(got) {
		return fmt.Errorf("generated output differs: %s (file set)", actual)
	}
	for name, data := range want {
		if other, ok := got[name]; !ok || !bytes.Equal(data, other) {
			return fmt.Errorf("generated output differs: %s", filepath.Join(actual, name))
		}
	}
	return nil
}

func hashAsset(dir, name string) (string, error) {
	path := filepath.Join(dir, "assets", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	ext := filepath.Ext(name)
	hashed := fmt.Sprintf("%s-%x%s", strings.TrimSuffix(name, ext), sum[:6], ext)
	return hashed, os.Rename(path, filepath.Join(dir, "assets", hashed))
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0644)
	})
}
