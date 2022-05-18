package fancyindex

import (
	_ "embed"
	"errors"
	"github.com/sirupsen/logrus"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html
var indexTemplateSource string

type FileServer struct {
	fileSystem    fs.FS
	indexTemplate *template.Template
}

func (f *FileServer) serveDir(dir fs.File, s fs.FileInfo, w http.ResponseWriter, r *http.Request) {
	d, ok := dir.(fs.ReadDirFile)
	if !ok {
		_, _ = w.Write([]byte("file does not readdir"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	listing, err := f.loadDirectoryContents(d, s.Name(), path.Clean(r.URL.Path))
	if err != nil {
		_, _ = w.Write([]byte("could not load directory contents"))
		return
	}
	f.browseApplyQueryParams(w, r, &listing)
	err = f.indexTemplate.Execute(w, listing)
	if err != nil {
		panic(err)
	}
}

func (f *FileServer) loadDirectoryContents(dir fs.ReadDirFile, root, urlPath string) (browseTemplateContext, error) {
	direntries, err := dir.ReadDir(-1)
	if err != nil {
		return browseTemplateContext{}, err
	}
	sort.Slice(direntries, func(i, j int) bool {
		return direntries[i].Name() < direntries[j].Name()
	})

	var files []os.FileInfo
	for _, direntry := range direntries {
		info, err := direntry.Info()
		if err != nil {
			logrus.Errorf("error reading info for %s: %s", direntry.Name(), err)
			continue
		}
		files = append(files, info)
	}

	// user can presumably browse "up" to parent folder if path is longer than "/"
	canGoUp := len(urlPath) > 1
	return f.directoryListing(files, canGoUp, root, urlPath), nil
}

func (f *FileServer) browseApplyQueryParams(w http.ResponseWriter, r *http.Request, listing *browseTemplateContext) {
	sortParam := r.URL.Query().Get("sort")
	orderParam := r.URL.Query().Get("order")
	limitParam := r.URL.Query().Get("limit")
	offsetParam := r.URL.Query().Get("offset")

	// first figure out what to sort by
	switch sortParam {
	case "":
		sortParam = sortByNameDirFirst
		if sortCookie, sortErr := r.Cookie("sort"); sortErr == nil {
			sortParam = sortCookie.Value
		}
	case sortByName, sortByNameDirFirst, sortBySize, sortByTime:
		http.SetCookie(w, &http.Cookie{Name: "sort", Value: sortParam, Secure: r.TLS != nil})
	}

	// then figure out the order
	switch orderParam {
	case "":
		orderParam = "asc"
		if orderCookie, orderErr := r.Cookie("order"); orderErr == nil {
			orderParam = orderCookie.Value
		}
	case "asc", "desc":
		http.SetCookie(w, &http.Cookie{Name: "order", Value: orderParam, Secure: r.TLS != nil})
	}

	// finally, apply the sorting and limiting
	listing.applySortAndLimit(sortParam, orderParam, limitParam, offsetParam)
}

func (f *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_path := r.URL.Path
	if len(_path) > 0 && _path[0] == '/' {
		_path = _path[1:]
	}
	if strings.HasSuffix(_path, "/") {
		_path = _path[:len(_path)-1]
	}

	if len(_path) == 0 {
		_path = "."
	}
	file, err := f.fileSystem.Open(_path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte("file not found"))
			return
		}
		if errors.Is(err, fs.ErrPermission) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte("permission denied"))
			return
		}
		panic(err)
	}
	defer func(file fs.File) {
		err = file.Close()
		if err != nil {
			logrus.Warning(err)
		}
	}(file)

	s, err := file.Stat()
	if err != nil {
		panic(err)
	}

	if s.IsDir() {
		f.serveDir(file, s, w, r)
		return
	}

	rs, ok := file.(io.ReadSeeker)
	if !ok {
		_, _ = w.Write([]byte("fs file does not support Seek"))
		return
	}
	if w.Header().Get("Content-Type") == "" {
		mtyp := mime.TypeByExtension(filepath.Ext(s.Name()))
		if mtyp == "" {
			mtyp = "application/octet-stream"
		}
	}
	http.ServeContent(w, r, s.Name(), s.ModTime(), rs)
}

func Browser(root string) http.Handler {
	indexTemplate, err := template.New("index").Funcs(template.FuncMap{
		"formatSize": func(s int64) string {
			prefixes := []string{"", "K", "M", "G", "T"}
			prefix := 0
			for s > 1000 && prefix < len(prefixes)-1 {
				s /= 1000
				prefix++
			}

			return strconv.FormatInt(s, 10) + " " + prefixes[prefix] + "B"
		},
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 15:04:05 -0700 MST")
		},
	}).Parse(indexTemplateSource)
	if err != nil {
		panic(err)
	}
	h := &FileServer{
		indexTemplate: indexTemplate,
	}
	h.fileSystem = os.DirFS(h.calculateAbsolutePath(root))
	return h
}
