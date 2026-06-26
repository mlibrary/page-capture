package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const captureAppleScript = `
on run argv
	if (count of argv) is not 4 then error "expected 4 args"

	set targetURLRaw to item 1 of argv
	set htmlPathRaw to item 2 of argv
	set cssPathRaw to item 3 of argv
	set pngPathRaw to item 4 of argv

	set targetURL to (targetURLRaw as text)
	set htmlPath to (htmlPathRaw as text)
	set cssPath to (cssPathRaw as text)
	set pngPath to (pngPathRaw as text)

	set targetURLUTF8 to (targetURL as «class utf8»)
	set htmlPathUTF8 to (htmlPath as «class utf8»)
	set cssPathUTF8 to (cssPath as «class utf8»)
	set pngPathUTF8 to (pngPath as «class utf8»)

	set targetWindow to missing value
	set htmlHandle to missing value
	set cssHandle to missing value

	try
		tell application "Safari"
			activate
			make new document with properties {URL:(targetURLUTF8 as text)}
			delay 0.3
			set targetWindow to front window
			set bounds of targetWindow to {10, 30, 1290, 1230}
			set targetTab to current tab of targetWindow

			set waited to 0
			set pageLoaded to false
			repeat while waited < 120
				delay 0.5
				set waited to waited + 1
				set pageReady to do JavaScript "document.readyState" in targetTab
				set pageURL to do JavaScript "location.href || ''" in targetTab
				if pageReady is "complete" and pageURL is not "" and pageURL is not "about:blank" then
					set pageLoaded to true
					exit repeat
				end if
			end repeat
			if pageLoaded is false then error "page did not finish loading"

			set pageDOM to (do JavaScript "(function(){if(!document.documentElement){return '';}var url=window.location.href||'';document.documentElement.setAttribute('data-source-url',url);var head=document.head||document.getElementsByTagName('head')[0];if(head){var meta=document.createElement('meta');meta.setAttribute('name','capture-source-url');meta.setAttribute('content',url);head.appendChild(meta);}var targetedProps=['width','height','color','background-color','display','position','margin','padding','font-size','font-family','flex-direction','grid-template-columns','opacity','visibility','z-index','box-sizing','overflow','text-align'];var excludedTags=['SCRIPT','STYLE','META','LINK','TEMPLATE','NOSCRIPT','HEAD','TITLE'];function compileFor(el){var computed=window.getComputedStyle(el);if(!computed){return '';}if(computed.getPropertyValue('display')==='none'){return '';}var out='';for(var i=0;i<targetedProps.length;i++){var prop=targetedProps[i];var value=computed.getPropertyValue(prop);if(!value){continue;}if(value==='none'||value==='normal'||value==='rgba(0, 0, 0, 0)'){continue;}if(prop==='position'&&value==='static'){continue;}if(prop==='display'&&value==='inline'){continue;}if(prop==='z-index'&&value==='auto'){continue;}if(prop==='visibility'&&value==='visible'){continue;}if((prop==='margin'||prop==='padding')&&(value==='0px'||value==='0px 0px')){continue;}if(prop==='overflow'&&value==='visible'){continue;}if(prop==='box-sizing'&&value==='content-box'){continue;}out += prop + ': ' + value + '; ';}return out.trim();}var htmlCSS=compileFor(document.documentElement);if(htmlCSS){document.documentElement.setAttribute('data-computed-css',htmlCSS);}var bodyElements=document.querySelectorAll('body, body *');for(var j=0;j<bodyElements.length;j++){var el=bodyElements[j];if(excludedTags.indexOf(el.tagName)>=0){continue;}var css=compileFor(el);if(css){el.setAttribute('data-computed-css',css);}}return btoa(unescape(encodeURIComponent(document.documentElement.outerHTML)));})()" in targetTab) as Unicode text
			set pageCSS to (do JavaScript "(function(){let c='';for(let s of document.querySelectorAll('style')){c+=(s.textContent||'')+'\\n';}for(let l of document.querySelectorAll('link[href]')){let rel=(l.rel||'').toLowerCase();if(!rel.includes('stylesheet')){continue;}let h=l.href||'';if(h){c += '@import url(' + JSON.stringify(h) + ');\\n';}}for(let s of document.styleSheets){try{for(let r of s.cssRules){c += r.cssText + '\\n';}}catch(e){}}return btoa(unescape(encodeURIComponent(c)));})()" in targetTab) as Unicode text
			set winBounds to bounds of targetWindow
		end tell

		set htmlHandle to open for access (POSIX file (htmlPathUTF8 as text)) with write permission
		set eof htmlHandle to 0
		write (pageDOM as «class utf8») to htmlHandle
		close access htmlHandle
		set htmlHandle to missing value

		set cssHandle to open for access (POSIX file (cssPathUTF8 as text)) with write permission
		set eof cssHandle to 0
		write (pageCSS as «class utf8») to cssHandle
		close access cssHandle
		set cssHandle to missing value

		set x1 to item 1 of winBounds
		set y1 to item 2 of winBounds
		set x2 to item 3 of winBounds
		set y2 to item 4 of winBounds
		set rectArg to (x1 as text) & "," & (y1 as text) & "," & ((x2 - x1) as text) & "," & ((y2 - y1) as text)

		set screenshotCmd to "screencapture -x -R " & quoted form of rectArg & " " & quoted form of (pngPathUTF8 as text)
		do shell script screenshotCmd

		tell application "Safari"
			if targetWindow is not missing value then close targetWindow
		end tell
	on error errMsg number errNum
		try
			if htmlHandle is not missing value then close access htmlHandle
		end try
		try
			if cssHandle is not missing value then close access cssHandle
		end try
		try
			tell application "Safari"
				if targetWindow is not missing value then close targetWindow
			end tell
		end try
		error "capture failed (" & errNum & "): " & errMsg
	end try
end run
`

func main() {
	targetDir := flag.String("target-dir", "", "Directory to write capture output")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s --target-dir <dir> <URL>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if strings.TrimSpace(*targetDir) == "" {
		fatalf("missing required --target-dir")
	}
	if flag.NArg() != 1 {
		fatalf("expected exactly one URL argument")
	}

	targetURL := flag.Arg(0)
	if err := validateURL(targetURL); err != nil {
		fatalf("invalid URL: %v", err)
	}

	absTargetDir, err := filepath.Abs(*targetDir)
	if err != nil {
		fatalf("resolve target dir: %v", err)
	}
	if err := os.MkdirAll(absTargetDir, 0o755); err != nil {
		fatalf("create target dir: %v", err)
	}

	htmlPath := filepath.Join(absTargetDir, "index.html")
	cssPath := filepath.Join(absTargetDir, "styles.css")
	pngPath := filepath.Join(absTargetDir, "screenshot.png")

	if err := capture(targetURL, htmlPath, cssPath, pngPath); err != nil {
		fatalf("%v", err)
	}

	if err := decodeBase64CaptureFile(htmlPath); err != nil {
		fatalf("decode html: %v", err)
	}
	if err := decodeBase64CaptureFile(cssPath); err != nil {
		fatalf("decode css: %v", err)
	}

	if err := injectStylesheetLink(htmlPath); err != nil {
		fatalf("link stylesheet: %v", err)
	}

	fmt.Printf("saved: %s\n", htmlPath)
	fmt.Printf("saved: %s\n", cssPath)
	fmt.Printf("saved: %s\n", pngPath)
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if strings.TrimSpace(u.Host) == "" {
		return errors.New("missing host")
	}
	return nil
}

func capture(targetURL, htmlPath, cssPath, pngPath string) error {
	cmd := exec.Command("osascript", "-", targetURL, htmlPath, cssPath, pngPath)
	cmd.Stdin = strings.NewReader(captureAppleScript)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("osascript failed: %s", msg)
	}
	return nil
}

func decodeBase64CaptureFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("decode base64 file %s: %w", filepath.Base(path), err)
	}

	return os.WriteFile(path, decoded, 0o644)
}

func injectStylesheetLink(htmlPath string) error {
	raw, err := os.ReadFile(htmlPath)
	if err != nil {
		return err
	}

	html := string(raw)
	if strings.Contains(html, `href="styles.css"`) {
		return nil
	}

	patched := strings.Replace(html, "</head>", `<link rel="stylesheet" href="styles.css">
</head>`, 1)
	if patched == html {
		return nil
	}

	return os.WriteFile(htmlPath, []byte(patched), 0o644)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
