package lib

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/imports"
)

type Generator struct {
	Fset          *token.FileSet
	ProjectName   string // e.g., "mylib_split"
	OutputDir     string // e.g., "./mylib_split"
	ThirdPartyDir string // e.g., "./mylib_split/third_party"
	ImportPrefix  string // e.g., "mylib_split/third_party"
}

func NewGenerator(inputFile string) *Generator {
	node, _ := parser.ParseFile(token.NewFileSet(), inputFile, nil, parser.PackageClauseOnly)
	pkgName := node.Name.Name + "_split"
	return &Generator{
		Fset:          token.NewFileSet(),
		ProjectName:   pkgName,
		OutputDir:     pkgName,
		ThirdPartyDir: filepath.Join(pkgName, "third_party"),
		ImportPrefix:  pkgName + "/third_party",
	}
}

// 1. AST MAPPING & REWRITING
// ---------------------------------------------------------

func (g *Generator) rewriteImportsInFile(file *ast.File) bool {
	changed := false
	for _, imp := range file.Imports {
		pathVal := strings.Trim(imp.Path.Value, `"`)
		if isThirdParty(pathVal) && !strings.HasPrefix(pathVal, g.ImportPrefix) {
			newPath := filepath.ToSlash(filepath.Join(g.ImportPrefix, pathVal))
			imp.Path.Value = fmt.Sprintf(`"%s"`, newPath)
			changed = true
		}
	}
	return changed
}

func (g *Generator) processDirectoryImports(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, err := parser.ParseFile(g.Fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}

		if g.rewriteImportsInFile(file) {
			f, err := os.Create(path)
			if err != nil {
				return err
			}
			defer f.Close()
			return format.Node(f, g.Fset, file)
		}
		return nil
	})
}

// 2. FILE GENERATION
// ---------------------------------------------------------

func (g *Generator) writeBucket(filename string, decls []ast.Decl, availableImports []*ast.ImportSpec) error {
	if len(decls) == 0 {
		return nil
	}

	var specs []ast.Spec
	for _, imp := range availableImports {
		specs = append(specs, imp)
	}

	newFile := &ast.File{
		Name:  ast.NewIdent(g.ProjectName),
		Decls: append([]ast.Decl{&ast.GenDecl{Tok: token.IMPORT, Specs: specs}}, decls...),
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, g.Fset, newFile); err != nil {
		return err
	}

	// Clean up unused imports immediately via goimports
	optimized, err := imports.Process(filename, buf.Bytes(), nil)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(g.OutputDir, filename), optimized, 0644)
}

// 3. DEPENDENCY MANAGEMENT
// ---------------------------------------------------------

func (g *Generator) setupThirdParty() error {
	// 1. Vendor
	if err := runCmd("", "go", "mod", "vendor"); err != nil {
		return err
	}
	defer os.RemoveAll("vendor")

	// 2. Identify modules from modules.txt
	f, err := os.Open("vendor/modules.txt")
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# ") {
			mod := strings.Fields(line)[1]
			oldPath := filepath.Join("vendor", mod)
			newPath := filepath.Join(g.ThirdPartyDir, mod)

			os.MkdirAll(filepath.Dir(newPath), 0755)
			if err := os.Rename(oldPath, newPath); err != nil {
				continue // Usually sub-packages already moved by parent
			}
		}
	}
	return nil
}

// 4. MAIN ORCHESTRATION
// ---------------------------------------------------------

func GenerateFiles(inputFile string) {
	g := NewGenerator(inputFile)
	fmt.Printf("🚀 Starting generation for %s...\n", g.ProjectName)

	node, err := parser.ParseFile(g.Fset, inputFile, nil, parser.ParseComments)
	if err != nil {
		panic(err)
	}

	var typeDecls, funcDecls, methodDecls []ast.Decl
	var allImports []*ast.ImportSpec

	for _, decl := range node.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				for _, s := range d.Specs {
					allImports = append(allImports, s.(*ast.ImportSpec))
				}
			} else {
				typeDecls = append(typeDecls, d)
			}
		case *ast.FuncDecl:
			if d.Recv == nil {
				funcDecls = append(funcDecls, d)
			} else {
				methodDecls = append(methodDecls, d)
			}
		}
	}

	os.MkdirAll(g.OutputDir, 0755)

	// Write split files
	base := filepath.Base(inputFile)
	g.writeBucket(base+"_types.go", typeDecls, allImports)
	g.writeBucket(base+"_funcs.go", funcDecls, allImports)
	g.writeBucket(base+"_methods.go", methodDecls, allImports)

	// Init module
	runCmd(g.OutputDir, "go", "mod", "init", g.ProjectName)

	// Setup deps
	if err := g.setupThirdParty(); err != nil {
		panic(err)
	}

	// Rewrite all imports (The Shading phase)
	fmt.Println("✏️  Rewriting imports to local paths...")
	g.processDirectoryImports(g.OutputDir)

	// Final Tidy
	runCmd(g.OutputDir, "go", "mod", "tidy")
	fmt.Println("✨ Done!")
}

// HELPERS
// ---------------------------------------------------------

func runCmd(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func isThirdParty(path string) bool {
	if path == "" {
		return false
	}
	firstPart := strings.Split(path, "/")[0]
	return strings.Contains(firstPart, ".")
}
