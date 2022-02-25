// Package vuln_ubuntu contains a ParseUbuntuRepository method to parse the Ubuntu repository
// to look out for Ubuntu releases that patch CVEs. It parses the changelogs from the metadata.
//
// It also contains a LoadUbuntuFixedCVEs to load the results of the parsing.
//
// Both the parsing and loading of results use sqlite3 as backend storage.
package vuln_ubuntu

import (
	"archive/tar"
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	kitlog "github.com/go-kit/kit/log"
	"github.com/gocolly/colly"
	_ "github.com/mattn/go-sqlite3"
	"github.com/ulikunitz/xz"
	"golang.org/x/sync/errgroup"
)

func defaultCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "fleet", "vuln", "ubuntu"), nil
}

// UbuntuPkg holds data to identify a Ubuntu package.
type Package struct {
	Name    string
	Version string
}

// FixedCVESet is a set of fixed CVEs.
type FixedCVEs map[Package][]string

// Add adds the given package and CVE/s to the set.
func (f FixedCVEs) Add(pkg Package, cves []string) {
	f[pkg] = append(f[pkg], cves...)
}

type UbuntuOpts struct {
	noCrawl  bool
	verbose  bool
	cacheDir string
	root     string
}

type UbuntuOption func(*UbuntuOpts)

func WithCacheDir(dir string) UbuntuOption {
	return func(o *UbuntuOpts) {
		o.cacheDir = dir
	}
}

func NoCrawl() UbuntuOption {
	return func(o *UbuntuOpts) {
		o.noCrawl = true
	}
}

func WithVerbose(v bool) UbuntuOption {
	return func(o *UbuntuOpts) {
		o.verbose = v
	}
}

func WithRoot(root string) UbuntuOption {
	return func(o *UbuntuOpts) {
		o.root = root
	}
}

const (
	repositoryDomain = "archive.ubuntu.com"
	repositoryURL    = "http://" + repositoryDomain
	defaultRoot      = "/ubuntu/pool/"
)

// ParseUbuntuRepository performs the following operations:
//   - Crawls the Ubuntu repository website. To find all the tar.xz files. Example http://archive.ubuntu.com/ubuntu/pool/universe/c/chromium-browser/chromium-browser_80.0.3987.163-0ubuntu1.tar.xz
//   - Downloads tar.xz files and finds the changelog in each package
//   - It parses the changelogs for each package release and looks for the "CVE-" string.
//
// It writes progress messages to stdout.
func ParseUbuntuRepository(opts ...UbuntuOption) (FixedCVEs, error) {
	var opts_ UbuntuOpts
	for _, fn := range opts {
		fn(&opts_)
	}

	if opts_.cacheDir == "" {
		var err error
		opts_.cacheDir, err = defaultCacheDir()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(opts_.cacheDir, 0700); err != nil {
			return nil, err
		}
	}

	fmt.Printf("Using cache directory: %s\n", opts_.cacheDir)
	if !opts_.noCrawl {
		if err := crawl(opts_.root, opts_.cacheDir, opts_.verbose); err != nil {
			return nil, err
		}
	}

	fixedCVEs, err := parse(opts_.cacheDir)
	if err != nil {
		return nil, err
	}

	return fixedCVEs, nil
}

func crawl(root string, cacheDir string, verbose bool) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	fmt.Println("Crawling Ubuntu repository...")

	pkgURLs := make(chan *url.URL)

	c := colly.NewCollector()
	c.OnHTML("td > a[href]", func(e *colly.HTMLElement) {
		href := e.Attr("href")

		// Don't visit parent dirs
		if len(href) > 0 && href[0] == '/' {
			return
		}

		if strings.HasSuffix(href, ".tar.xz") {
			u := *e.Request.URL // clone the url
			u.Path = path.Join(u.Path, href)
			pkgURLs <- &u
			if verbose {
				fmt.Printf("%s\n", u.Path)
			}
			return
		}
		if !strings.Contains(href, "/") {
			return
		}
		e.Request.Visit(href)
	})

	c.AllowedDomains = append(c.AllowedDomains, repositoryDomain)

	if root == "" {
		root = defaultRoot
	}

	g := new(errgroup.Group)
	g.Go(func() error {
		defer close(pkgURLs)
		return c.Visit(repositoryURL + root)
	})

	// Start a fixed number of goroutines to download .tar.xz files, extract, and save changelogs
	numDownloaders := 10
	for i := 0; i < numDownloaders; i++ {
		g.Go(func() error {
			for u := range pkgURLs {
				if err := processPKGURL(u, cacheDir, verbose); err != nil {
					return err
				}
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func parse(cacheDir string) (FixedCVEs, error) {
	fmt.Println("Processing package changelog files...")

	fixedCVEs := make(FixedCVEs)

	var pkg Package
	err := filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if filepath.Dir(path) == cacheDir {
			// top level dir, parse package
			rel, err := filepath.Rel(cacheDir, path)
			if err != nil {
				return err
			}

			// TODO: extract function to parse package name version
			parts := strings.SplitN(rel, "_", 2)
			if len(parts) != 2 {
				return errors.New("parse package name and version")
			}
			pkg = Package{
				Name:    parts[0],
				Version: parts[1],
			}

			return nil
		}

		if d.IsDir() || filepath.Base(path) != "changelog" {
			// shouldn't happen
			return nil
		}

		cves, err := parseChangelog(path)
		if err != nil {
			return err
		}

		fixedCVEs.Add(pkg, cves)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return fixedCVEs, nil
}

func processPKGURL(u *url.URL, parentDir string, verbose bool) error {
	destDir := filepath.Join(parentDir, strings.TrimSuffix(filepath.Base(u.Path), ".tar.xz"))

	if _, err := os.Stat(destDir); err == nil {
		if verbose {
			fmt.Printf("skipping %s, already exists\n", destDir)
		}
		return nil
	}

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("non 200 status code: %d", resp.StatusCode)
	}

	xzr, err := xz.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("create xz reader: %w", err)
	}

	tr := tar.NewReader(xzr)
	for {
		header, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				return nil // end of archive
			}
			return fmt.Errorf("tar: %w", err)
		}

		info := header.FileInfo()
		if info.IsDir() {
			// skip dirs for now...
			continue
		}

		// only write changelog files
		filename := header.Name
		if filepath.Base(filename) != "changelog" {
			continue
		}

		destFileName := filepath.Join(destDir, filename)

		// create dirs
		if err := os.MkdirAll(filepath.Dir(destFileName), 0o755); err != nil {
			return err
		}

		err = func() error {
			f, err := os.Create(destFileName)
			if err != nil {
				return err
			}
			defer f.Close()

			_, err = io.Copy(f, tr)
			return err
		}()
		if err != nil {
			return err
		}

	}

	return nil
}

var cveRegex = regexp.MustCompile(`CVE\-[0-9]{4}\-[0-9]{4,}`)

func parseChangelog(filename string) ([]string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// dedup cves
	cvesMap := make(map[string]struct{})

	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		matches := cveRegex.FindAllString(scanner.Text(), -1)
		for _, m := range matches {
			cvesMap[m] = struct{}{}
		}
	}

	cves := make([]string, 0, len(cvesMap))
	for cve := range cvesMap {
		cves = append(cves, cve)
	}

	return cves, nil
}

const UbuntuFixedCVEsTable = "ubuntu_fixed_cves"

// LoadUbuntuFixedCVEs loads the Ubuntu packages with known fixed CVEs from the given sqlite3 db.
func LoadUbuntuFixedCVEs(ctx context.Context, db *sql.DB, logger kitlog.Logger) (map[Package]map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT name, version, cves FROM %s`, UbuntuFixedCVEsTable))
	if err != nil {
		return nil, fmt.Errorf("fetch packages: %w", err)
	}
	defer rows.Close()

	fixedCVEsByPackage := make(map[Package]map[string]struct{})
	for rows.Next() {
		var pkg Package
		var cvesStr string
		if err := rows.Scan(&pkg.Name, &pkg.Version, &cvesStr); err != nil {
			return nil, fmt.Errorf("scan package: %w")
		}
		cves := strings.Split(cvesStr, ",")

		cvesMap, ok := fixedCVEsByPackage[pkg]
		if !ok {
			cvesMap = make(map[string]struct{})
		}
		for _, cve := range cves {
			cvesMap[cve] = struct{}{}
		}
		fixedCVEsByPackage[pkg] = cvesMap
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch packages: %w", err)
	}

	return fixedCVEsByPackage, nil
}

// GenUbuntuSqlite will store the Ubuntu package set in the given sqlite db.
func GenUbuntuSqlite(db *sql.DB, fixedCVEs FixedCVEs) error {
	if err := createTable(db); err != nil {
		return err
	}

	query := fmt.Sprintf(`
REPLACE INTO %s (name, version, cves)
VALUES (?, ?, ?)
`, UbuntuFixedCVEsTable)

	for pkg, cves := range fixedCVEs {
		cvesStr := strings.Join(cves, ",")
		_, err := db.Exec(query, pkg.Name, pkg.Version, cvesStr)
		if err != nil {
			return err
		}
	}

	return nil
}

func createTable(db *sql.DB) error {
	_, err := db.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    name TEXT,
    version TEXT,
    cves TEXT,
    UNIQUE (name, version)
)`, UbuntuFixedCVEsTable))
	return err
}
