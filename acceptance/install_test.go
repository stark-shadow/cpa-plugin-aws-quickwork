package acceptance

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)

func TestArchiveWithCPAInstaller(t *testing.T) {
	path := os.Getenv("QUICKWORK_INSTALL_ARCHIVE")
	if path == "" {
		t.Skip("set QUICKWORK_INSTALL_ARCHIVE to a built ZIP")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) []byte {
		file, err := archive.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	var metadata struct{ ID, Version string }
	if json.Unmarshal(read("manifest.json"), &metadata) != nil || metadata.ID != "aws-quickwork" || metadata.Version == "" {
		t.Fatal("invalid manifest")
	}
	var descriptor struct {
		Version string `json:"plugin_version"`
	}
	if json.Unmarshal(read(metadata.ID+".omc.json"), &descriptor) != nil || descriptor.Version != metadata.Version {
		t.Fatal("invalid OMC descriptor")
	}
	platform, extension, magic := "linux", ".so", "\x7fELF"
	if strings.HasSuffix(path, "_windows_amd64.zip") {
		platform, extension, magic = "windows", ".dll", "MZ"
	}
	library := read(metadata.ID + extension)
	if !bytes.HasPrefix(library, []byte(magic)) {
		t.Fatal("library format does not match artifact platform")
	}
	serve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer serve.Close()
	client := pluginstore.NewClient(serve.Client(), "")
	manifest := pluginstore.Manifest{SchemaVersion: pluginstore.SchemaVersionV2, ID: metadata.ID, Version: metadata.Version, Install: pluginstore.InstallPlan{
		Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{{GOOS: platform, GOARCH: "amd64", URL: serve.URL + "/" + filepath.Base(path), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}},
	}}
	dir := t.TempDir()
	options := pluginstore.InstallOptions{PluginsDir: dir, GOOS: platform, GOARCH: "amd64"}
	result, err := client.InstallManifest(t.Context(), manifest, options)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, platform, "amd64", metadata.ID+"-v"+metadata.Version+extension)
	deployed, err := os.ReadFile(result.Path)
	if err != nil || result.Path != want || !bytes.Equal(deployed, library) {
		t.Fatalf("wrong installed artifact at %q: %v", result.Path, err)
	}
	manifest.Install.Artifacts[0].SHA256 = strings.Repeat("0", 64)
	if _, err := client.InstallManifest(t.Context(), manifest, options); err == nil {
		t.Fatal("installer accepted invalid checksum")
	}
}
