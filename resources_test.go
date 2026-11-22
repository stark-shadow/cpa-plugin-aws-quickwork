package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestOMCResourceRPC(t *testing.T) {
	var runtime engine
	runtime.swap(&settings{Enabled: true})
	t.Cleanup(runtime.stop)
	value, failure := runtime.call(pluginabi.MethodManagementRegister, nil, nil)
	if failure != nil {
		t.Fatalf("registration: %v", failure)
	}
	var registered struct {
		Routes    []struct{ Method, Path string }
		Resources []struct{ Path string }
	}
	if json.Unmarshal(raw(value), &registered) != nil || len(registered.Routes) != 1 || registered.Routes[0].Path != discoveryRoute || registered.Routes[0].Method != http.MethodPost || len(registered.Resources) != 3 || registered.Resources[0].Path != "/omc.json" {
		t.Fatalf("unexpected resource registration: %+v", registered)
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, descriptorPath, http.StatusOK},
		{http.MethodPost, descriptorPath, http.StatusMethodNotAllowed},
		{http.MethodGet, descriptorPath + "/../auth.json", http.StatusNotFound},
	} {
		value, failure := runtime.call(pluginabi.MethodManagementHandle, raw(pluginapi.ManagementRequest{Method: test.method, Path: test.path}), nil)
		if failure != nil {
			t.Fatalf("%s %s: %v", test.method, test.path, failure)
		}
		response := value.(pluginapi.ManagementResponse)
		if response.StatusCode != test.status {
			t.Fatalf("%s %s: got %d, want %d", test.method, test.path, response.StatusCode, test.status)
		}
		if test.status != http.StatusOK {
			if len(response.Body) != 0 {
				t.Fatal("rejected resource request returned content")
			}
			continue
		}
		var descriptor struct {
			SchemaVersion int    `json:"schema_version"`
			PluginID      string `json:"plugin_id"`
			PluginVersion string `json:"plugin_version"`
		}
		if err := json.Unmarshal(response.Body, &descriptor); err != nil || descriptor.SchemaVersion != 1 || descriptor.PluginID != id || descriptor.PluginVersion != version {
			t.Fatalf("invalid descriptor: %+v (%v)", descriptor, err)
		}
		if response.Headers.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("unexpected headers: %v", response.Headers)
		}
	}
}
