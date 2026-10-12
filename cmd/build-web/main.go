// Command build-web bundles the browser console with the Go esbuild API.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := buildWeb(*rootFlag); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func buildWeb(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
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
	if err := os.RemoveAll(out); err != nil {
		return fmt.Errorf("remove old web/dist: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return fmt.Errorf("install web/dist: %w", err)
	}
	embedded := filepath.Join(root, "cmd", "tosub2", "web", "dist")
	if err := os.RemoveAll(embedded); err != nil {
		return fmt.Errorf("remove embedded dist: %w", err)
	}
	if err := copyTree(out, embedded); err != nil {
		return fmt.Errorf("copy embedded dist: %w", err)
	}
	fmt.Printf("built %s and %s\n", out, embedded)
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
