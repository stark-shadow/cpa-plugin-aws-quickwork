package main

import (
	"html/template"
	"net/http"
	"strings"
)

type callbackCopy struct {
	step, title, body, action string
}

var callbackTemplate = template.Must(template.New("callback").Parse(`<!doctype html>
<html lang="{{.Language}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · Amazon QuickWork</title>
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif}body{margin:0;display:grid;min-height:100vh;place-items:center;background:Canvas;color:CanvasText}main{box-sizing:border-box;width:min(34rem,calc(100% - 2rem));padding:clamp(1.5rem,5vw,3rem);border:1px solid GrayText;border-radius:1.25rem}small{font-weight:700;color:GrayText}h1{font-size:clamp(1.45rem,5vw,2rem);line-height:1.2}p{line-height:1.6}a{display:inline-block;margin-top:.5rem;padding:.6rem 1rem;border-radius:.5rem;background:#0d766f;color:#fff;text-decoration:none}a:focus-visible{outline:3px solid Highlight;outline-offset:3px}
</style></head><body><main><small>Amazon QuickWork · {{.Step}}</small><h1>{{.Title}}</h1><p>{{.Body}}</p>{{if .URL}}<a href="{{.URL}}">{{.Action}}</a>{{end}}</main></body></html>`))

func callbackPage(w http.ResponseWriter, req *http.Request, status int, stage, next string) {
	language := "en"
	copy := callbackCopy{"Step 2 of 2", "This sign-in link is no longer valid", "Return to the CPA OAuth page and start a new QuickWork sign-in.", ""}
	chinese := strings.HasPrefix(strings.ToLower(strings.TrimSpace(req.Header.Get("Accept-Language"))), "zh")
	if chinese {
		language = "zh-CN"
		copy = callbackCopy{"第 2 步，共 2 步", "此登录链接已失效", "请返回 CPA 的 OAuth 登录页，重新发起 QuickWork 登录。", ""}
	}
	switch stage {
	case "discover":
		copy = callbackCopy{"Step 1 of 2", "Preparing QuickWork authorization", "Keep the CPA sign-in window open. This page will continue automatically. If it doesn't, select Continue.", "Continue"}
		if chinese {
			copy = callbackCopy{"第 1 步，共 2 步", "正在准备 QuickWork 授权", "请保持 CPA 登录弹窗开启。本页会自动进入下一步；如果没有跳转，可点击继续。", "继续"}
		}
	case "received":
		copy = callbackCopy{"Step 2 of 2", "Authorization received", "Return to CPA and wait for it to confirm that your credential was saved. You can close this browser tab.", ""}
		if chinese {
			copy = callbackCopy{"第 2 步，共 2 步", "已收到授权", "请返回 CPA，等待面板确认凭据已保存。可以关闭此浏览器标签页。", ""}
		}
	case "denied":
		copy = callbackCopy{"Step 2 of 2", "Authorization was not completed", "Return to CPA to retry if you still want to connect this account.", ""}
		if chinese {
			copy = callbackCopy{"第 2 步，共 2 步", "授权未完成", "如需连接此账号，请返回 CPA 重试。", ""}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = callbackTemplate.Execute(w, struct{ Language, Step, Title, Body, Action, URL string }{language, copy.step, copy.title, copy.body, copy.action, next})
}
