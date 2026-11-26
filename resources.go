package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const publicResourceBase = "/v0/resource/plugins/" + id
const descriptorPath = publicResourceBase + "/omc.json"
const rawAssetBaseURL = "https://raw.githubusercontent.com/stark-shadow/cpa-plugin-aws-quickwork/main/assets"
const pluginLogoURL = rawAssetBaseURL + "/logo.svg"
const personalLogoURL = rawAssetBaseURL + "/personal.svg"
const enterpriseLogoURL = rawAssetBaseURL + "/enterprise.svg"

//go:embed aws-quickwork.omc.json
var omcDescriptor string

//go:embed assets/logo.svg
var pluginLogoSVG string

//go:embed assets/personal.svg
var personalLogo string

//go:embed assets/enterprise.svg
var enterpriseLogo string

func publicResource(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body, media string
	switch req.Path {
	case descriptorPath:
		body, media = omcDescriptor, "application/json; charset=utf-8"
	case publicResourceBase + "/logo.svg":
		body, media = pluginLogoSVG, "image/svg+xml"
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
