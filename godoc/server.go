// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package godoc

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/token"
	htmlpkg "html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"code.google.com/p/go.tools/godoc/util"
	"code.google.com/p/go.tools/godoc/vfs"
	"code.google.com/p/go.tools/godoc/vfs/httpfs"
)

// TODO(bradfitz,adg): these are moved from godoc.go globals.
// Clean this up.
var (
	FileServer http.Handler // default file server
	CmdHandler Server
	PkgHandler Server

	// file system information
	FSTree      util.RWValue // *Directory tree of packages, updated with each sync (but sync code is removed now)
	FSModified  util.RWValue // timestamp of last call to invalidateIndex
	DocMetadata util.RWValue // mapping from paths to *Metadata
)

func InitHandlers(p *Presentation) {
	c := p.Corpus
	FileServer = http.FileServer(httpfs.New(c.fs))
	CmdHandler = Server{p, c, "/cmd/", "/src/cmd"}
	PkgHandler = Server{p, c, "/pkg/", "/src/pkg"}
}

// Server is a godoc server.
type Server struct {
	p       *Presentation
	c       *Corpus // copy of p.Corpus
	pattern string  // url pattern; e.g. "/pkg/"
	fsRoot  string  // file system root to which the pattern is mapped
}

func (s *Server) FSRoot() string { return s.fsRoot }

func (s *Server) RegisterWithMux(mux *http.ServeMux) {
	mux.Handle(s.pattern, s)
}

// getPageInfo returns the PageInfo for a package directory abspath. If the
// parameter genAST is set, an AST containing only the package exports is
// computed (PageInfo.PAst), otherwise package documentation (PageInfo.Doc)
// is extracted from the AST. If there is no corresponding package in the
// directory, PageInfo.PAst and PageInfo.PDoc are nil. If there are no sub-
// directories, PageInfo.Dirs is nil. If an error occurred, PageInfo.Err is
// set to the respective error but the error is not logged.
//
func (h *Server) GetPageInfo(abspath, relpath string, mode PageInfoMode) *PageInfo {
	info := &PageInfo{Dirname: abspath}

	// Restrict to the package files that would be used when building
	// the package on this system.  This makes sure that if there are
	// separate implementations for, say, Windows vs Unix, we don't
	// jumble them all together.
	// Note: Uses current binary's GOOS/GOARCH.
	// To use different pair, such as if we allowed the user to choose,
	// set ctxt.GOOS and ctxt.GOARCH before calling ctxt.ImportDir.
	ctxt := build.Default
	ctxt.IsAbsPath = pathpkg.IsAbs
	ctxt.ReadDir = fsReadDir
	ctxt.OpenFile = fsOpenFile
	pkginfo, err := ctxt.ImportDir(abspath, 0)
	// continue if there are no Go source files; we still want the directory info
	if _, nogo := err.(*build.NoGoError); err != nil && !nogo {
		info.Err = err
		return info
	}

	// collect package files
	pkgname := pkginfo.Name
	pkgfiles := append(pkginfo.GoFiles, pkginfo.CgoFiles...)
	if len(pkgfiles) == 0 {
		// Commands written in C have no .go files in the build.
		// Instead, documentation may be found in an ignored file.
		// The file may be ignored via an explicit +build ignore
		// constraint (recommended), or by defining the package
		// documentation (historic).
		pkgname = "main" // assume package main since pkginfo.Name == ""
		pkgfiles = pkginfo.IgnoredGoFiles
	}

	// get package information, if any
	if len(pkgfiles) > 0 {
		// build package AST
		fset := token.NewFileSet()
		files, err := parseFiles(fset, abspath, pkgfiles)
		if err != nil {
			info.Err = err
			return info
		}

		// ignore any errors - they are due to unresolved identifiers
		pkg, _ := ast.NewPackage(fset, files, poorMansImporter, nil)

		// extract package documentation
		info.FSet = fset
		if mode&ShowSource == 0 {
			// show extracted documentation
			var m doc.Mode
			if mode&NoFiltering != 0 {
				m = doc.AllDecls
			}
			if mode&AllMethods != 0 {
				m |= doc.AllMethods
			}
			info.PDoc = doc.New(pkg, pathpkg.Clean(relpath), m) // no trailing '/' in importpath

			// collect examples
			testfiles := append(pkginfo.TestGoFiles, pkginfo.XTestGoFiles...)
			files, err = parseFiles(fset, abspath, testfiles)
			if err != nil {
				log.Println("parsing examples:", err)
			}
			info.Examples = collectExamples(pkg, files)

			// collect any notes that we want to show
			if info.PDoc.Notes != nil {
				// could regexp.Compile only once per godoc, but probably not worth it
				if rx, err := regexp.Compile(NotesRx); err == nil {
					for m, n := range info.PDoc.Notes {
						if rx.MatchString(m) {
							if info.Notes == nil {
								info.Notes = make(map[string][]*doc.Note)
							}
							info.Notes[m] = n
						}
					}
				}
			}

		} else {
			// show source code
			// TODO(gri) Consider eliminating export filtering in this mode,
			//           or perhaps eliminating the mode altogether.
			if mode&NoFiltering == 0 {
				packageExports(fset, pkg)
			}
			info.PAst = ast.MergePackageFiles(pkg, 0)
		}
		info.IsMain = pkgname == "main"
	}

	// get directory information, if any
	var dir *Directory
	var timestamp time.Time
	if tree, ts := FSTree.Get(); tree != nil && tree.(*Directory) != nil {
		// directory tree is present; lookup respective directory
		// (may still fail if the file system was updated and the
		// new directory tree has not yet been computed)
		dir = tree.(*Directory).lookup(abspath)
		timestamp = ts
	}
	if dir == nil {
		// no directory tree present (too early after startup or
		// command-line mode); compute one level for this page
		// note: cannot use path filter here because in general
		//       it doesn't contain the FSTree path
		dir = h.c.newDirectory(abspath, 1)
		timestamp = time.Now()
	}
	info.Dirs = dir.listing(true)
	info.DirTime = timestamp
	info.DirFlat = mode&FlatDir != 0

	return info
}

func (h *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if redirect(w, r) {
		return
	}

	relpath := pathpkg.Clean(r.URL.Path[len(h.pattern):])
	abspath := pathpkg.Join(h.fsRoot, relpath)
	mode := GetPageInfoMode(r)
	if relpath == BuiltinPkgPath {
		mode = NoFiltering
	}
	info := h.GetPageInfo(abspath, relpath, mode)
	if info.Err != nil {
		log.Print(info.Err)
		h.p.ServeError(w, r, relpath, info.Err)
		return
	}

	if mode&NoHTML != 0 {
		h.p.ServeText(w, applyTemplate(PackageText, "packageText", info))
		return
	}

	var tabtitle, title, subtitle string
	switch {
	case info.PAst != nil:
		tabtitle = info.PAst.Name.Name
	case info.PDoc != nil:
		tabtitle = info.PDoc.Name
	default:
		tabtitle = info.Dirname
		title = "Directory "
		if ShowTimestamps {
			subtitle = "Last update: " + info.DirTime.String()
		}
	}
	if title == "" {
		if info.IsMain {
			// assume that the directory name is the command name
			_, tabtitle = pathpkg.Split(relpath)
			title = "Command "
		} else {
			title = "Package "
		}
	}
	title += tabtitle

	// special cases for top-level package/command directories
	switch tabtitle {
	case "/src/pkg":
		tabtitle = "Packages"
	case "/src/cmd":
		tabtitle = "Commands"
	}

	h.p.ServePage(w, Page{
		Title:    title,
		Tabtitle: tabtitle,
		Subtitle: subtitle,
		Body:     applyTemplate(PackageHTML, "packageHTML", info),
	})
}

type PageInfoMode uint

const (
	NoFiltering PageInfoMode = 1 << iota // do not filter exports
	AllMethods                           // show all embedded methods
	ShowSource                           // show source code, do not extract documentation
	NoHTML                               // show result in textual form, do not generate HTML
	FlatDir                              // show directory in a flat (non-indented) manner
)

// modeNames defines names for each PageInfoMode flag.
var modeNames = map[string]PageInfoMode{
	"all":     NoFiltering,
	"methods": AllMethods,
	"src":     ShowSource,
	"text":    NoHTML,
	"flat":    FlatDir,
}

// GetPageInfoMode computes the PageInfoMode flags by analyzing the request
// URL form value "m". It is value is a comma-separated list of mode names
// as defined by modeNames (e.g.: m=src,text).
func GetPageInfoMode(r *http.Request) PageInfoMode {
	var mode PageInfoMode
	for _, k := range strings.Split(r.FormValue("m"), ",") {
		if m, found := modeNames[strings.TrimSpace(k)]; found {
			mode |= m
		}
	}
	return AdjustPageInfoMode(r, mode)
}

// AdjustPageInfoMode allows specialized versions of godoc to adjust
// PageInfoMode by overriding this variable.
var AdjustPageInfoMode = func(_ *http.Request, mode PageInfoMode) PageInfoMode {
	return mode
}

// fsReadDir implements ReadDir for the go/build package.
func fsReadDir(dir string) ([]os.FileInfo, error) {
	return FS.ReadDir(filepath.ToSlash(dir))
}

// fsOpenFile implements OpenFile for the go/build package.
func fsOpenFile(name string) (r io.ReadCloser, err error) {
	data, err := vfs.ReadFile(FS, filepath.ToSlash(name))
	if err != nil {
		return nil, err
	}
	return ioutil.NopCloser(bytes.NewReader(data)), nil
}

// poorMansImporter returns a (dummy) package object named
// by the last path component of the provided package path
// (as is the convention for packages). This is sufficient
// to resolve package identifiers without doing an actual
// import. It never returns an error.
//
func poorMansImporter(imports map[string]*ast.Object, path string) (*ast.Object, error) {
	pkg := imports[path]
	if pkg == nil {
		// note that strings.LastIndex returns -1 if there is no "/"
		pkg = ast.NewObj(ast.Pkg, path[strings.LastIndex(path, "/")+1:])
		pkg.Data = ast.NewScope(nil) // required by ast.NewPackage for dot-import
		imports[path] = pkg
	}
	return pkg, nil
}

// globalNames returns a set of the names declared by all package-level
// declarations. Method names are returned in the form Receiver_Method.
func globalNames(pkg *ast.Package) map[string]bool {
	names := make(map[string]bool)
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			addNames(names, decl)
		}
	}
	return names
}

// collectExamples collects examples for pkg from testfiles.
func collectExamples(pkg *ast.Package, testfiles map[string]*ast.File) []*doc.Example {
	var files []*ast.File
	for _, f := range testfiles {
		files = append(files, f)
	}

	var examples []*doc.Example
	globals := globalNames(pkg)
	for _, e := range doc.Examples(files...) {
		name := stripExampleSuffix(e.Name)
		if name == "" || globals[name] {
			examples = append(examples, e)
		} else {
			log.Printf("skipping example 'Example%s' because '%s' is not a known function or type", e.Name, e.Name)
		}
	}

	return examples
}

// addNames adds the names declared by decl to the names set.
// Method names are added in the form ReceiverTypeName_Method.
func addNames(names map[string]bool, decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		name := d.Name.Name
		if d.Recv != nil {
			var typeName string
			switch r := d.Recv.List[0].Type.(type) {
			case *ast.StarExpr:
				typeName = r.X.(*ast.Ident).Name
			case *ast.Ident:
				typeName = r.Name
			}
			name = typeName + "_" + name
		}
		names[name] = true
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names[s.Name.Name] = true
			case *ast.ValueSpec:
				for _, id := range s.Names {
					names[id.Name] = true
				}
			}
		}
	}
}

// packageExports is a local implementation of ast.PackageExports
// which correctly updates each package file's comment list.
// (The ast.PackageExports signature is frozen, hence the local
// implementation).
//
func packageExports(fset *token.FileSet, pkg *ast.Package) {
	for _, src := range pkg.Files {
		cmap := ast.NewCommentMap(fset, src, src.Comments)
		ast.FileExports(src)
		src.Comments = cmap.Filter(src).Comments()
	}
}

func applyTemplate(t *template.Template, name string, data interface{}) []byte {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Printf("%s.Execute: %s", name, err)
	}
	return buf.Bytes()
}

func redirect(w http.ResponseWriter, r *http.Request) (redirected bool) {
	canonical := pathpkg.Clean(r.URL.Path)
	if !strings.HasSuffix(canonical, "/") {
		canonical += "/"
	}
	if r.URL.Path != canonical {
		url := *r.URL
		url.Path = canonical
		http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
		redirected = true
	}
	return
}

func redirectFile(w http.ResponseWriter, r *http.Request) (redirected bool) {
	c := pathpkg.Clean(r.URL.Path)
	c = strings.TrimRight(c, "/")
	if r.URL.Path != c {
		url := *r.URL
		url.Path = c
		http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
		redirected = true
	}
	return
}

func (p *Presentation) serveTextFile(w http.ResponseWriter, r *http.Request, abspath, relpath, title string) {
	src, err := vfs.ReadFile(p.Corpus.fs, abspath)
	if err != nil {
		log.Printf("ReadFile: %s", err)
		p.ServeError(w, r, relpath, err)
		return
	}

	if r.FormValue("m") == "text" {
		p.ServeText(w, src)
		return
	}

	var buf bytes.Buffer
	buf.WriteString("<pre>")
	FormatText(&buf, src, 1, pathpkg.Ext(abspath) == ".go", r.FormValue("h"), RangeSelection(r.FormValue("s")))
	buf.WriteString("</pre>")
	fmt.Fprintf(&buf, `<p><a href="/%s?m=text">View as plain text</a></p>`, htmlpkg.EscapeString(relpath))

	p.ServePage(w, Page{
		Title:    title + " " + relpath,
		Tabtitle: relpath,
		Body:     buf.Bytes(),
	})
}

func (p *Presentation) serveDirectory(w http.ResponseWriter, r *http.Request, abspath, relpath string) {
	if redirect(w, r) {
		return
	}

	list, err := FS.ReadDir(abspath)
	if err != nil {
		p.ServeError(w, r, relpath, err)
		return
	}

	p.ServePage(w, Page{
		Title:    "Directory " + relpath,
		Tabtitle: relpath,
		Body:     applyTemplate(DirlistHTML, "dirlistHTML", list),
	})
}

func (p *Presentation) ServeHTMLDoc(w http.ResponseWriter, r *http.Request, abspath, relpath string) {
	// get HTML body contents
	src, err := vfs.ReadFile(FS, abspath)
	if err != nil {
		log.Printf("ReadFile: %s", err)
		p.ServeError(w, r, relpath, err)
		return
	}

	// if it begins with "<!DOCTYPE " assume it is standalone
	// html that doesn't need the template wrapping.
	if bytes.HasPrefix(src, doctype) {
		w.Write(src)
		return
	}

	// if it begins with a JSON blob, read in the metadata.
	meta, src, err := extractMetadata(src)
	if err != nil {
		log.Printf("decoding metadata %s: %v", relpath, err)
	}

	// evaluate as template if indicated
	if meta.Template {
		tmpl, err := template.New("main").Funcs(TemplateFuncs).Parse(string(src))
		if err != nil {
			log.Printf("parsing template %s: %v", relpath, err)
			p.ServeError(w, r, relpath, err)
			return
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, nil); err != nil {
			log.Printf("executing template %s: %v", relpath, err)
			p.ServeError(w, r, relpath, err)
			return
		}
		src = buf.Bytes()
	}

	// if it's the language spec, add tags to EBNF productions
	if strings.HasSuffix(abspath, "go_spec.html") {
		var buf bytes.Buffer
		Linkify(&buf, src)
		src = buf.Bytes()
	}

	p.ServePage(w, Page{
		Title:    meta.Title,
		Subtitle: meta.Subtitle,
		Body:     src,
	})
}

func (p *Presentation) serveFile(w http.ResponseWriter, r *http.Request) {
	relpath := r.URL.Path

	// Check to see if we need to redirect or serve another file.
	if m := MetadataFor(relpath); m != nil {
		if m.Path != relpath {
			// Redirect to canonical path.
			http.Redirect(w, r, m.Path, http.StatusMovedPermanently)
			return
		}
		// Serve from the actual filesystem path.
		relpath = m.filePath
	}

	abspath := relpath
	relpath = relpath[1:] // strip leading slash

	switch pathpkg.Ext(relpath) {
	case ".html":
		if strings.HasSuffix(relpath, "/index.html") {
			// We'll show index.html for the directory.
			// Use the dir/ version as canonical instead of dir/index.html.
			http.Redirect(w, r, r.URL.Path[0:len(r.URL.Path)-len("index.html")], http.StatusMovedPermanently)
			return
		}
		p.ServeHTMLDoc(w, r, abspath, relpath)
		return

	case ".go":
		p.serveTextFile(w, r, abspath, relpath, "Source file")
		return
	}

	dir, err := FS.Lstat(abspath)
	if err != nil {
		log.Print(err)
		p.ServeError(w, r, relpath, err)
		return
	}

	if dir != nil && dir.IsDir() {
		if redirect(w, r) {
			return
		}
		if index := pathpkg.Join(abspath, "index.html"); util.IsTextFile(FS, index) {
			p.ServeHTMLDoc(w, r, index, index)
			return
		}
		p.serveDirectory(w, r, abspath, relpath)
		return
	}

	if util.IsTextFile(FS, abspath) {
		if redirectFile(w, r) {
			return
		}
		p.serveTextFile(w, r, abspath, relpath, "Text file")
		return
	}

	FileServer.ServeHTTP(w, r)
}

func serveSearchDesc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	data := map[string]interface{}{
		"BaseURL": fmt.Sprintf("http://%s", r.Host),
	}
	if err := SearchDescXML.Execute(w, &data); err != nil && err != http.ErrBodyNotAllowed {
		// Only log if there's an error that's not about writing on HEAD requests.
		// See Issues 5451 and 5454.
		log.Printf("searchDescXML.Execute: %s", err)
	}
}

func (p *Presentation) ServeText(w http.ResponseWriter, text []byte) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(text)
}
