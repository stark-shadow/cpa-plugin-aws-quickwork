package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const publicResourceBase = "/v0/resource/plugins/" + id
const descriptorPath = publicResourceBase + "/omc.json"

//go:embed aws-quickwork.omc.json
var omcDescriptor string

const personalLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="12" fill="#0d766f"/><g fill="none" stroke="#fff" stroke-width="3" stroke-linecap="round"><circle cx="24" cy="17" r="5"/><path d="M14 34v-2a10 8 0 0 1 20 0v2"/></g></svg>`

const enterpriseLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><rect width="48" height="48" rx="12" fill="#3c4596"/><g fill="none" stroke="#fff" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M13 35h22M17 35V14h14v21m-7-1v-5m-2-10h4m-4 5h4"/></g></svg>`

func publicResource(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body, media string
	switch req.Path {
	case descriptorPath:
		body, media = omcDescriptor, "application/json; charset=utf-8"
	case publicResourceBase + "/personal.svg":
		body, media = personalLogo, "image/svg+xml"
	case publicResourceBase + "/enterprise.svg":
		body, media = enterpriseLogo, "image/svg+xml"
	default:
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
	}
	if req.Method != http.MethodGet {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    http.Header{"Allow": {http.MethodGet}},
		}
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {media},
			"Cache-Control":           {"no-cache"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'"},
		},
		Body: []byte(body),
	}
}
