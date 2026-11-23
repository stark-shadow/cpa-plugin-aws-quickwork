package acceptance

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	var metadata struct {
		ID, Version string
		Platforms   []string
	}
	if json.Unmarshal(read("manifest.json"), &metadata) != nil || metadata.ID != "aws-quickwork" || metadata.Version == "" {
		t.Fatal("invalid manifest")
	}
	var descriptor struct {
		Version string `json:"plugin_version"`
	}
	if json.Unmarshal(read(metadata.ID+".omc.json"), &descriptor) != nil || descriptor.Version != metadata.Version {
		t.Fatal("invalid OMC descriptor")
	}
	base := filepath.Base(path)
	prefix := metadata.ID + "_" + metadata.Version + "_"
	target, ok := strings.CutPrefix(base, prefix)
	platform, arch, separated := strings.Cut(strings.TrimSuffix(target, ".zip"), "_")
	if !ok || !separated || !strings.HasSuffix(target, ".zip") || !slices.Contains(metadata.Platforms, platform+"/"+arch) {
		t.Fatalf("unexpected archive filename or undeclared platform: %s", base)
	}
	extension := map[string]string{"linux": ".so", "darwin": ".dylib", "windows": ".dll"}[platform]
	if extension == "" {
		t.Fatalf("unsupported platform: %s", platform)
	}
	wanted := []string{metadata.ID + extension, "manifest.json", metadata.ID + ".omc.json", "README.md", "README.zh-CN.md", "LICENSE"}
	var entries []string
	for _, file := range archive.File {
		entries = append(entries, file.Name)
	}
	slices.Sort(entries)
	slices.Sort(wanted)
	if !slices.Equal(entries, wanted) {
		t.Fatalf("unexpected ZIP entries: got %v, want %v", entries, wanted)
	}
	library := read(metadata.ID + extension)
	verifyLibrary(t, library, platform, arch)
	t.Logf("Verified %s for %s/%s", base, platform, arch)
	serve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer serve.Close()
	client := pluginstore.NewClient(serve.Client(), "")
	manifest := pluginstore.Manifest{SchemaVersion: pluginstore.SchemaVersionV2, ID: metadata.ID, Version: metadata.Version, Install: pluginstore.InstallPlan{
		Type: pluginstore.InstallTypeDirect, Artifacts: []pluginstore.Artifact{{GOOS: platform, GOARCH: arch, URL: serve.URL + "/" + filepath.Base(path), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))}},
	}}
	dir := t.TempDir()
	options := pluginstore.InstallOptions{PluginsDir: dir, GOOS: platform, GOARCH: arch}
	result, err := client.InstallManifest(t.Context(), manifest, options)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, platform, arch, metadata.ID+"-v"+metadata.Version+extension)
	deployed, err := os.ReadFile(result.Path)
	if err != nil || result.Path != want || !bytes.Equal(deployed, library) {
		t.Fatalf("wrong installed artifact at %q: %v", result.Path, err)
	}
	manifest.Install.Artifacts[0].SHA256 = strings.Repeat("0", 64)
	if _, err := client.InstallManifest(t.Context(), manifest, options); err == nil {
		t.Fatal("installer accepted invalid checksum")
	}
}

func verifyLibrary(t *testing.T, library []byte, platform, arch string) {
	t.Helper()
	reader := bytes.NewReader(library)
	switch platform {
	case "linux":
		f, err := elf.NewFile(reader)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type != elf.ET_DYN || f.Class != elf.ELFCLASS64 || f.Machine != elf.EM_X86_64 || arch != "amd64" {
			t.Fatalf("expected Linux amd64 shared library, got %s %s %s", f.Type, f.Class, f.Machine)
		}
	case "darwin":
		f, err := macho.NewFile(reader)
		if err != nil {
			t.Fatal(err)
		}
		cpu, supported := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[arch]
		if !supported || f.Type != macho.TypeDylib || f.Magic != macho.Magic64 || f.Cpu != cpu {
			t.Fatalf("expected macOS %s dynamic library, got %s %s (magic %#x)", arch, f.Type, f.Cpu, f.Magic)
		}
	case "windows":
		f, err := pe.NewFile(reader)
		if err != nil {
			t.Fatal(err)
		}
		_, is64 := f.OptionalHeader.(*pe.OptionalHeader64)
		if !is64 || f.Characteristics&pe.IMAGE_FILE_DLL == 0 || f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || arch != "amd64" {
			t.Fatalf("expected Windows amd64 DLL, got machine %#x and characteristics %#x", f.Machine, f.Characteristics)
		}
	}
}
