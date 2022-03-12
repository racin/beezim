package indexer

import (
	"archive/tar"
	"bytes"
	"embed"
	"errors"
	"html/template"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/r0qs/beezim/internal/tarball"

	zim "github.com/akhenakh/gozim"
	"github.com/cheggaaa/pb/v3"
	"github.com/ethersphere/bee/pkg/swarm"
)

//go:embed assets/*
var assetsFS embed.FS

//go:embed templates/*
var templateFS embed.FS

var templates *template.Template

func init() {
	var err error

	templates, err = template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		log.Fatal("error parsing templates:", err)
	}
}

type Article struct {
	path string
	data []byte
}

func (a Article) Path() string {
	return a.path
}

func (a Article) Data() []byte {
	return a.Data()
}

type SwarmWikiIndexer struct {
	mu      sync.Mutex
	ZimPath string
	Z       *zim.ZimReader
	entries map[string]IndexEntry // RELATIVE_PATH or ArticleID -> METADATA ?
	root    swarm.Address         // TODO: hash of the root manifest metadata (if empty, not uploaded)
}

// TODO: store root in a local kv db pointing to the metadata in swarm
// or maybe in a feed and parse the feed on load to collect all root pages and their metadata.

type IndexEntry struct {
	Path     string
	Metadata map[string]string
}

func New(zimPath string) (*SwarmWikiIndexer, error) {
	z, err := zim.NewReader(zimPath, false)
	if err != nil {
		return nil, err
	}

	return &SwarmWikiIndexer{
		ZimPath: zimPath,
		Z:       z,
		entries: make(map[string]IndexEntry),
	}, nil
}

func (idx *SwarmWikiIndexer) AddEntry(entryPath string, metadata map[string]string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries[entryPath] = IndexEntry{
		Path:     entryPath,
		Metadata: metadata,
	}
}

func (idx *SwarmWikiIndexer) Entries() map[string]IndexEntry {
	return idx.entries
}

func (idx *SwarmWikiIndexer) ParseZIM() chan Article {
	zimArticles := make(chan Article)
	go func() {
		defer close(zimArticles)
		progressBar := pb.New(int(idx.Z.ArticleCount))
		progressBar.Set(pb.Bytes, true)
		progressBar.Start()

		log.Printf("Parsing zim file: %s", filepath.Base(idx.ZimPath))
		start := time.Now()
		idx.Z.ListTitlesPtrIterator(func(i uint32) {
			a, err := idx.Z.ArticleAtURLIdx(i)
			if err != nil || a.EntryType == zim.DeletedEntry {
				return
			}

			// FIXME: for now, all namespaces are considered equal when parsing
			// https://openzim.org/wiki/ZIM_file_format
			var data []byte
			switch a.Namespace {
			case '-', // Assets (CSS, JS, Favicon)
				'A', // Text files (Article Format)
				'I', // Media files
				'M', // ZIM Metadata
				'X': // Search indexes (Xapian db)

				if a.EntryType == zim.RedirectEntry {
					ridx, err := a.RedirectIndex()
					if err != nil {
						return
					}
					ra, err := idx.Z.ArticleAtURLIdx(ridx)
					if err != nil {
						return
					}
					data, err = buildRedirectPage(path.Base(ra.FullURL()))
					if err != nil {
						log.Fatalf("error building redirect page: %v", err)
					}
				} else {
					data, err = a.Data()
					if err != nil {
						return
					}
				}

				zimArticles <- Article{
					path: a.FullURL(),
					data: data,
				}

				// TODO: add addresses and searchable data
				idx.AddEntry(a.FullURL(), map[string]string{
					"Title":    a.Title,
					"MimeType": a.MimeType(),
				})

				// TODO: For now we are ignoring some cases, but we should create "_exceptions/" directory in case of errors extracting the files like is done by the zim-tools.
				// https://github.com/openzim/zim-tools/blob/a26a450110e9ca2ec1b20de8237a3bd382af71f5/src/zimdump.cpp#L214
			default:
			}
			progressBar.Increment()
		})
		progressBar.Finish()
		elapsed := time.Since(start)
		log.Printf("File processed in %v", elapsed)
	}()
	return zimArticles
}

func (idx *SwarmWikiIndexer) UnZim(outputDir string, files <-chan Article) error {
	if _, err := os.Stat(outputDir); os.IsNotExist(err) {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return err
		}
	}

	for file := range files {
		filePath := filepath.Join(outputDir, file.path)
		fileDirPath := filepath.Dir(filePath)

		if _, err := os.Stat(fileDirPath); os.IsNotExist(err) {
			if err := os.MkdirAll(fileDirPath, 0755); err != nil {
				return err
			}
		}

		f, err := os.Create(filePath)
		if err != nil {
			return err
		}

		if _, err := f.Write(file.data); err != nil {
			return err
		}

		f.Close()
	}

	return nil
}

func (idx *SwarmWikiIndexer) TarZim(tarFile string, files <-chan Article) error {
	f, err := os.Create(tarFile)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	for file := range files {
		hdr := &tar.Header{
			Name: file.path,
			Mode: 0600,
			Size: int64(len(file.data)),
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if _, err := tw.Write(file.data); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return nil
}

func buildRedirectPage(pagePath string) ([]byte, error) {
	tmplData := map[string]interface{}{
		"MainURL": pagePath,
	}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "index-redirect.html", tmplData); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MakeRedirectIndexPage creates an redirect index to the main page
// when it exists in the zim archive.
func (idx *SwarmWikiIndexer) MakeRedirectIndexPage(tarFile string) error {
	mainPage, err := idx.Z.MainPage()
	if err != nil {
		return err
	}

	// TODO: handle the case where there is no main page in the article.
	// Should we add an index and browse all articles?
	if mainPage == nil {
		return errors.New("no index found in the ZIM")
	}

	var buf bytes.Buffer
	data, err := buildRedirectPage(mainPage.FullURL())
	if err != nil {
		return err
	}
	_, err = buf.Write(data)
	if err != nil {
		return err
	}
	return tarball.AppendTarData(tarFile, tarball.NewBufferFile("index.html", &buf))
}

// MakeIndexSearchPage creates a custom index with the text search tool and
// embed the current main page in the new index.
func (idx *SwarmWikiIndexer) MakeIndexSearchPage(tarFile string) error {
	mainPage, err := idx.Z.MainPage()
	if err != nil {
		return err
	}

	mainURL := ""
	if mainPage != nil {
		mainURL = mainPage.FullURL()
	}

	tmplData := map[string]interface{}{
		"File":        filepath.Base(idx.ZimPath),
		"Count":       strconv.Itoa(int(idx.Z.ArticleCount)),
		"Articles":    idx.entries,
		"HasMainPage": (mainURL != ""),
		"MainURL":     mainURL,
	}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "index-search.html", tmplData); err != nil {
		return err
	}
	return tarball.AppendTarData(tarFile, tarball.NewBufferFile("index.html", &buf))
}

// MakeErrorPage creates a custom error page
func (idx *SwarmWikiIndexer) MakeErrorPage(tarFile string) error {
	tmplData := map[string]interface{}{
		"File": filepath.Base(idx.ZimPath),
	}
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "error.html", tmplData); err != nil {
		return err
	}
	return tarball.AppendTarData(tarFile, tarball.NewBufferFile("error.html", &buf))
}
