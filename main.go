package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-github/v39/github"
	"golang.org/x/oauth2"
)

var owner = flag.String("owner", "", "Owner of the repo with the release asset")
var repo = flag.String("repo", "", "Repo with the release asset")
var binaryVersion = flag.String("version", "", "Version of the release asset to fetch, if unset, use latest")
var assetPattern = flag.String("asset-pattern", "", "Pattern the asset name must match")
var installPath = flag.String("install-path", "", "Where to put the installed binary")
var verbose = flag.Bool("verbose", false, "whether to enable verbose logging")
var token = flag.String("token", "", "Github token to use for authentication")

var githubToken = os.Getenv("GITHUB_TOKEN")
var githubPath = os.Getenv("GITHUB_PATH")

func main() {
	// make sure that the required flags and env vars are set
	flag.Parse()
	validateFlags()

	if githubToken == "" {
		// this is used by the GH client transparently
		log.Fatalf("GITHUB_TOKEN must be set")
	}
	if githubPath == "" {
		// this is used to add the installed binary to the actions path
		log.Fatalf("GITHUB_PATH must be set")
	}

	// check that we can use the supplied pattern to match assets
	assetPatternRegexp, err := regexp.Compile(strings.TrimSpace(*assetPattern))
	if err != nil {
		log.Fatalf("asset-pattern (%s) was not a valid regexp: %s", *assetPattern, err)
	}

	httpClient := &http.Client{}
	httpRequestCtx := context.Background()

	if *token != "" {
		httpClient = oauth2.NewClient(httpRequestCtx, oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: *token,
			TokenType:   "Bearer",
		}))
	}
	client := github.NewClient(httpClient)

	// list releases for the repo
	log.Printf("listing releases for %s/%s", *owner, *repo)
	var release *github.RepositoryRelease
	if *binaryVersion == "" {
		// if there is no version, then use the latest
		releases, _, err := client.Repositories.ListReleases(httpRequestCtx, *owner, *repo, nil)
		if err != nil {
			log.Fatalf("Failed to get releases: %s", err)
		}
		if len(releases) == 0 {
			log.Fatalf("There were no releases for this repo")
		}
		release = releases[0]

		if *verbose {
			log.Printf("using release: %s", *release.Name)
		}
	} else {
		// if version is set, then look up the release by tag
		release, _, err = client.Repositories.GetReleaseByTag(httpRequestCtx, *owner, *repo, *binaryVersion)
		if err != nil {
			log.Fatalf("Failed to get releases: %s", err)
		}
		if *verbose {
			log.Printf("using release: %s", *release.Name)
		}
	}

	// find the asset to download from a number of release assets
	var asset *github.ReleaseAsset
	for _, v := range release.Assets {
		if *verbose {
			log.Printf("checking asset with name: %s", *v.Name)
		}
		if assetPatternRegexp.MatchString(*(v.Name)) {
			asset = v
			if *verbose {
				log.Printf("selected asset with name: %s", *v.Name)
			}
			break
		}
	}
	if asset == nil {
		log.Fatalf("No matching release assets found")
	}

	// download the asset to a tempdir
	log.Printf("downloading matching asset: %s", *asset.Name)
	rc, _, err := client.Repositories.DownloadReleaseAsset(httpRequestCtx, *owner, *repo, *asset.ID, httpClient)
	if err != nil {
		log.Fatalf("failed to get release asset: %s", err)
	}

	dir, err := ioutil.TempDir("", "release-asset-")
	if err != nil {
		log.Fatalf("failed to make tempdir: %s", err)
	}
	defer rc.Close()
	defer os.RemoveAll(dir)

	// extract the download if needed
	var binaryPath string
	if strings.HasSuffix(*asset.Name, ".tar.gz") {
		log.Println("unpacking tar.gz to temp dir")

		err = untar(dir, rc)
		if err != nil {
			log.Fatalf("failed to untar data: %s", err)
		}

		binaryItems := []string{}
		items, _ := ioutil.ReadDir(dir)
		for _, item := range items {
			itemPath := fmt.Sprintf("%s/%s", dir, item.Name())

			file, err := os.Open(itemPath)
			if err != nil {
				log.Fatal(err)
			}
			defer file.Close()

			byteSlice := make([]byte, 512)
			_, err = file.Read(byteSlice)
			if err != nil {
				log.Printf("failed to read file '%s': %s", item.Name(), err)
				continue
			}

			if item.Name() == "README.md" {
				continue
			}
			if item.Name() == "LICENSE" {
				continue
			}

			if http.DetectContentType(byteSlice) == "application/octet-stream" {
				if *verbose {
					log.Printf("selected binary '%s' from tar", item.Name())
				}
				binaryItems = append(binaryItems, itemPath)
			}
		}

		if len(binaryItems) != 1 {
			log.Fatalf("single binary expected, got %d: ", len(binaryItems))
		}

		binaryPath = binaryItems[0]
	} else {
		// otherwise, assume that the asset is the binary
		binaryPath = fmt.Sprintf("%s/binary", dir)
		out, err := os.Create(binaryPath)
		if err != nil {
			log.Fatalf("failed to write binary to temp path: %s", err)
		}
		defer out.Close()
		io.Copy(out, rc)
	}

	// move the downloaded binary to the installPath
	err = os.Rename(binaryPath, *installPath)
	if err != nil {
		log.Fatalf("failed to move binary to desired output path: %s", err)
	}

	// double check that the binary is executable
	err = os.Chmod(*installPath, 0755)
	if err != nil {
		log.Fatalf("failed to set binary as executable: %s", err)
	}

	// add the new binary to the GITHUB_PATH
	f, err := os.OpenFile(githubPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("failed to open GH path: %s", err)
	}
	defer f.Close()
	if _, err := f.WriteString(filepath.Dir(*installPath) + "\n"); err != nil {
		log.Fatalf("failed to update GH path: %s", err)
	}
}

func validateFlags() {
	if *owner == "" {
		log.Fatalf("owner flag must be set")
	}
	if *repo == "" {
		log.Fatalf("repo flag must be set")
	}
	if *assetPattern == "" {
		log.Fatalf("asset-pattern flag must be set")
	}
	if *installPath == "" {
		log.Fatalf("installPath flag must be set")
	}
}

// https://gist.githubusercontent.com/sdomino/635a5ed4f32c93aad131/raw/1f1a2609f9bf04f3a681a96c26350b0d694549bf/untargz.go
func untar(dst string, r io.Reader) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()

		switch {

		// if no more files are found return
		case err == io.EOF:
			return nil

		// return any other error
		case err != nil:
			return err

		// if the header is nil, just skip it (not sure how this happens)
		case header == nil:
			continue
		}

		// the target location where the dir/file should be created
		target := filepath.Join(dst, header.Name)

		// the following switch could also be done using fi.Mode(), not sure if there
		// a benefit of using one vs. the other.
		// fi := header.FileInfo()

		// check the file type
		switch header.Typeflag {

		// if its a dir and it doesn't exist create it
		case tar.TypeDir:
			if _, err := os.Stat(target); err != nil {
				if err := os.MkdirAll(target, 0755); err != nil {
					return err
				}
			}

		// if it's a file create it
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			// copy over contents
			if _, err := io.Copy(f, tr); err != nil {
				return err
			}

			// manually close here after each file operation; defering would cause each file close
			// to wait until all operations have completed.
			f.Close()
		}
	}
}
