package interp

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/traefik/yaegi/extract"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// GetImports parses Go code and returns a list of imported packages.
func GetImports(code string) []string {
	fset := token.NewFileSet()

	// Parse the code into an AST
	node, err := parser.ParseFile(fset, "", code, parser.ImportsOnly)
	if err != nil {
		return []string{}
	}

	var imports []string
	for _, imp := range node.Imports {
		packageName := strings.Trim(imp.Path.Value, `"`)
		if !IsStandardPackage(packageName) {
			imports = append(imports, packageName)
		}
	}

	return imports
}

// isStandardPackage checks if a package belongs to the Go standard library.
func IsStandardPackage(pkg string) bool {
	regex := regexp.MustCompile(`[a-zA-Z0-9_]+(\.([a-zA-Z0-9_])+)+(\/([a-zA-Z0-9_])+)+`)
	return !regex.MatchString(pkg)
}

// DownloadNonStandardPackages downloads all non-standard imports.
func DownloadNonStandardPackages(code string, interp *Interpreter, dependencies *sync.Map, lock *sync.Mutex) error {
	packages := GetImports(code)
	if len(packages) == 0 {
		return nil
	}
	if dependencies == nil {
		dependencies = new(sync.Map)
	}
	done := new(sync.Map)
	wg := new(sync.WaitGroup)
	for _, pkg := range packages {

		_, ok := dependencies.Load(pkg)
		if ok {
			continue
		}
		dependencies.Store(pkg, true)
		wg.Add(1)
		go func(pkg, targetDir string) {
			defer wg.Done()
			DownloadPackage(pkg, targetDir, interp, dependencies, lock, done)
		}(pkg, interp.opt.context.GOPATH+"/pkg/mod")
	}
	wg.Wait()
	return nil
}

func DownloadPackage(pkg string, targetDir string, interp *Interpreter, dependencies *sync.Map, lock *sync.Mutex, done *sync.Map) (string, error) {
	dependencies.Store(pkg, true)
	ep, err := module.EscapePath(pkg)
	if err != nil {
		return "", fmt.Errorf("failed to escape package path %s: %w", pkg, err)
	}
	base := "https://proxy.golang.org/" + ep + "/@"
	slog.Info("Downloading package", "package", pkg, "base", base)
	res, err := http.Get(base + "latest")
	if err != nil {
		return "", fmt.Errorf("failed to download package %s: %w", pkg, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response for package %s: %w", pkg, err)
	}
	if res.StatusCode != http.StatusOK {
		sp := strings.Split(ep, "/")
		ep, err = module.EscapePath(strings.Join(sp[0:len(sp)-1], "/"))
		if err != nil {
			return "", nil
		}
		res, err = http.Get("https://proxy.golang.org/" + ep + "/@latest")
		if err != nil {
			return "", nil
		}
		defer res.Body.Close()
		data, err = io.ReadAll(res.Body)
		if err != nil {
			return "", nil
		}
		if res.StatusCode != http.StatusOK {
			return "", nil
		}
	}
	dataMap := make(map[string]any)
	if err := json.Unmarshal(data, &dataMap); err != nil {
		return "", fmt.Errorf("failed to unmarshal JSON for package %s: %w", pkg, err)
	}
	if _, ok := dataMap["Version"]; !ok {
		return "", fmt.Errorf("package %s not found", pkg)
	}
	outDir := filepath.Join(targetDir, ep+fmt.Sprintf("@%s", dataMap["Version"]))
	res, err = http.Get("https://proxy.golang.org/" + ep + "/@v/" + dataMap["Version"].(string) + ".zip")
	if err != nil {
		return "", fmt.Errorf("failed to download package %s version %s: %w", ep, dataMap["Version"], err)
	}
	defer res.Body.Close()
	data, err = io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read zip for package %s: %w", ep, err)
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory for package %s: %w", ep, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("failed to read zip for package %s: %w", ep, err)
	}
	for _, e := range zr.File {
		f := e
		dir := strings.TrimSuffix(f.Name, filepath.Base(f.Name))

		_ = os.MkdirAll(targetDir+dir, 0755)
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open file %s in package %s: %w", f.Name, ep, err)
		}
		dat, err := io.ReadAll(rc)
		if err != nil {
			rc.Close()
			return "", fmt.Errorf("failed to read file %s in package %s: %w", f.Name, ep, err)
		}
		defer rc.Close()
		outFile, err := os.Create(filepath.Join(targetDir, f.Name))
		if err != nil {
			return "", fmt.Errorf("failed to create output file %s: %w", f.Name, err)
		}
		defer outFile.Close()
		if _, err := io.Copy(outFile, bytes.NewReader(dat)); err != nil {
			return "", fmt.Errorf("failed to copy file %s in package %s: %w", f.Name, ep, err)
		}
		if strings.Contains(f.Name, "go.mod") {
			m, err := modfile.Parse("go.mod", dat, nil)
			if err != nil {
				return "", fmt.Errorf("failed to parse go.mod for package %s: %w", ep, err)
			}
			for _, r := range m.Require {

				res, err = http.Get("https://proxy.golang.org/" + r.Mod.Path + "/@v/" + r.Mod.Version + ".zip")
				if err != nil {
					return "", fmt.Errorf("failed to download package %s version %s: %w", ep, dataMap["Version"], err)
				}
				defer res.Body.Close()
				data, err = io.ReadAll(res.Body)
				if err != nil {
					return "", fmt.Errorf("failed to read zip for package %s: %w", ep, err)
				}

				if err := os.MkdirAll(outDir, 0755); err != nil {
					return "", fmt.Errorf("failed to create directory for package %s: %w", ep, err)
				}
				zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					return "", fmt.Errorf("failed to read zip for package %s: %w", ep, err)
				}
				for _, e := range zr.File {
					f := e
					dir := strings.TrimSuffix(f.Name, filepath.Base(f.Name))

					_ = os.MkdirAll(targetDir+dir, 0755)
					if f.FileInfo().IsDir() {
						continue
					}
					rc, err := f.Open()
					if err != nil {
						return "", fmt.Errorf("failed to open file %s in package %s: %w", f.Name, ep, err)
					}
					dat, err := io.ReadAll(rc)
					if err != nil {
						rc.Close()
						return "", fmt.Errorf("failed to read file %s in package %s: %w", f.Name, ep, err)
					}
					defer rc.Close()
					outFile, err := os.Create(filepath.Join(targetDir, f.Name))
					if err != nil {
						return "", fmt.Errorf("failed to create output file %s: %w", f.Name, err)
					}
					defer outFile.Close()
					if _, err := io.Copy(outFile, bytes.NewReader(dat)); err != nil {
						return "", fmt.Errorf("failed to copy file %s in package %s: %w", f.Name, ep, err)
					}
				}
			}

		}
	}

	lock.Lock()
	_, ok := done.Load(pkg)
	if ok {
		return "", nil
	}
	done.Store(pkg, true)
	_ = ExtractPackage(pkg, targetDir, interp)
	lock.Unlock()
	return "", nil
}

func ExtractPackage(pkg string, location string, interp *Interpreter) error {
	ep, err := module.EscapePath(pkg)
	if err != nil {
		return fmt.Errorf("failed to escape package path %s: %w", pkg, err)
	}
	base := "https://proxy.golang.org/" + ep + "/@"
	res, err := http.Get(base + "latest")
	if err != nil {
		return fmt.Errorf("failed to download package %s: %w", pkg, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read response for package %s: %w", pkg, err)
	}
	if res.StatusCode != http.StatusOK {
		slog.Info("Package not found, trying without base package", "package", ep)
		sp := strings.Split(ep, "/")
		ep, err = module.EscapePath(strings.Join(sp[0:len(sp)-1], "/"))
		if err != nil {
			slog.Error("Failed to escape package path", "error", err)
			return nil
		}
		fmt.Println("Downloading Package:", ep)
		res, err = http.Get("https://proxy.golang.org/" + ep + "/@latest")
		if err != nil {
			slog.Error("Failed to download package", "error", err)
			return nil
		}
		defer res.Body.Close()
		data, err = io.ReadAll(res.Body)
		if err != nil {
			slog.Error("Failed to read response", "error", err)
			return nil
		}
		if res.StatusCode != http.StatusOK {
			slog.Error("Failed to download package", "error", res.Status)
			return nil
		}
	}
	dataMap := make(map[string]any)
	if err := json.Unmarshal(data, &dataMap); err != nil {
		slog.Error("Failed to unmarshal JSON", "error", err)
		return fmt.Errorf("failed to unmarshal JSON for package %s: %w", pkg, err)
	}
	if _, ok := dataMap["Version"]; !ok {
		return fmt.Errorf("package %s not found", pkg)
	}
	slog.Info("Package version", "version", dataMap["Version"])
	//pkgDir := ep + fmt.Sprintf("@%s", dataMap["Version"])
	outDir := filepath.Join(location, ep+fmt.Sprintf("@%s", dataMap["Version"]))
	if err = os.Chdir(outDir); err != nil {
		slog.Info("CHdir failed", "package", ep, "version", dataMap["Version"], "outdir", outDir)
		return nil
	}
	split := strings.Split(pkg, "/")
	dest := split[len(split)-1]
	if strings.Contains(dest, ".") {
		dest = strings.Split(dest, ".")[0]
	}
	//reg := regexp.MustCompile(`v[0-9]+`)
	//if reg.MatchString(dest) {
	//	dest = split[len(split)-2]
	///}
	ext := extract.Extractor{
		Dest: dest,
	}
	r := strings.NewReplacer("/", "-", ".", "_", "~", "_")
	oFile := r.Replace(pkg) + ".go"
	_ = os.Remove(oFile)
	slog.Info("Extracting package", "package", pkg, "location", location)
	var buf bytes.Buffer
	_, err = ext.Extract("./", ep, &buf)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	slog.Info("Number of bytes extracted", "bytes", buf.Len())

	slog.Info("Extracted package", "package", pkg, "output", oFile)
	f, err := os.Create(oFile)
	if err != nil {
		return err
	}

	n, err := io.Copy(f, &buf)
	if err != nil {
		_ = f.Close()
		return err
	}
	slog.Info("Wrote", slog.Int64("bytes", n), "to file", oFile)
	if err := f.Close(); err != nil {
		return err
	}
	if buf.Len() == 0 {
		slog.Info("No content extracted for package", "package", pkg)
		_ = os.Remove(oFile)
		ext.Dest = split[len(split)-2]
		_, err = ext.Extract("./", ep, &buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		slog.Info("Number of bytes extracted", "bytes", buf.Len())

		slog.Info("Extracted package", "package", pkg, "output", oFile)
		f, err := os.Create(oFile)
		if err != nil {
			return err
		}

		n, err := io.Copy(f, &buf)
		if err != nil {
			_ = f.Close()
			return err
		}
		slog.Info("Wrote", slog.Int64("bytes", n), "to file", oFile)
		if err := f.Close(); err != nil {
			return err
		}
		if buf.Len() == 0 {
			_ = os.Remove(oFile)
			return nil
		}
	}
	if err := interp.Use(interp.Symbols(pkg)); err != nil {
		slog.Error("Failed to use symbols", "error", err)
		return fmt.Errorf("failed to use symbols for package %s: %w", pkg, err)
	}

	return nil
}
