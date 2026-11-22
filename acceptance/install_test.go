package acceptance

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

func TestArchiveWithCPAInstaller(t *testing.T) {
	path := os.Getenv("QUICKWORK_INSTALL_ARCHIVE")
	if path == "" {
		t.Skip("set QUICKWORK_INSTALL_ARCHIVE to the built ZIP")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"LICENSE", "README.md", "README.zh-CN.md", "aws-quickwork.so", "manifest.json"}) {
		t.Fatalf("wrong archive entries: %v", names)
	}
	f, e := z.Open("aws-quickwork.so")
	if e != nil {
		t.Fatal(e)
	}
	lib, e := io.ReadAll(f)
	f.Close()
	if e != nil || len(lib) < 4 || string(lib[:4]) != "\x7fELF" {
		t.Fatal("archive is not shipping an ELF library")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(b))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aws-quickwork_0.1.0_linux_amd64.zip" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	client := pluginstore.NewClient(srv.Client(), "")
	manifest := pluginstore.Manifest{SchemaVersion: pluginstore.SchemaVersionV2, ID: "aws-quickwork", Version: "0.1.0", Install: pluginstore.InstallPlan{
		Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{{GOOS: "linux", GOARCH: "amd64", URL: srv.URL + "/aws-quickwork_0.1.0_linux_amd64.zip", SHA256: digest, Size: int64(len(b))}},
	}}
	dir := t.TempDir()
	opts := pluginstore.InstallOptions{PluginsDir: dir, GOOS: "linux", GOARCH: "amd64"}
	result, err := client.InstallManifest(t.Context(), manifest, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "linux", "amd64", "aws-quickwork-v0.1.0.so")
	written, err := os.ReadFile(result.Path)
	if err != nil || result.Path != want || !bytes.Equal(written, lib) {
		t.Fatalf("unexpected installed file: %q (%v)", result.Path, err)
	}
	t.Logf("CPA direct installer verified %s, matching linux/amd64 library SHA256=%x", filepath.Base(path), sha256.Sum256(written))
	manifest.Install.Artifacts[0].SHA256 = fmt.Sprintf("%064x", 0)
	if _, e := client.InstallManifest(t.Context(), manifest, opts); e == nil {
		t.Fatal("installer accepted an invalid checksum")
	}
}
