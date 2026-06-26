package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	viewportWidth  = 1280
	viewportHeight = 1200
)

type browserBackend string

const (
	backendSafari browserBackend = "safari"
	backendChrome browserBackend = "chrome"
)

const captureDOMJS = `(function(){if(!document.documentElement){return '';}var url=window.location.href||'';var browser='__CAPTURE_BROWSER__';document.documentElement.setAttribute('data-source-url',url);document.documentElement.setAttribute('data-browser',browser);var head=document.head||document.getElementsByTagName('head')[0];if(head){var meta=document.createElement('meta');meta.setAttribute('name','capture-source-url');meta.setAttribute('content',url);head.appendChild(meta);}var targetedProps=['width','height','color','background-color','display','position','margin','padding','font-size','font-family','flex-direction','grid-template-columns','opacity','visibility','z-index','box-sizing','overflow','text-align'];var excludedTags=['SCRIPT','STYLE','META','LINK','TEMPLATE','NOSCRIPT','HEAD','TITLE'];function compileFor(el){var computed=window.getComputedStyle(el);if(!computed){return '';}if(computed.getPropertyValue('display')==='none'){return '';}var out='';for(var i=0;i<targetedProps.length;i++){var prop=targetedProps[i];var value=computed.getPropertyValue(prop);if(!value){continue;}if(value==='none'||value==='normal'||value==='rgba(0, 0, 0, 0)'){continue;}if(prop==='position'&&value==='static'){continue;}if(prop==='display'&&value==='inline'){continue;}if(prop==='z-index'&&value==='auto'){continue;}if(prop==='visibility'&&value==='visible'){continue;}if((prop==='margin'||prop==='padding')&&(value==='0px'||value==='0px 0px')){continue;}if(prop==='overflow'&&value==='visible'){continue;}if(prop==='box-sizing'&&value==='content-box'){continue;}out += prop + ': ' + value + '; ';}return out.trim();}var htmlCSS=compileFor(document.documentElement);if(htmlCSS){document.documentElement.setAttribute('data-computed-css',htmlCSS);}var bodyElements=document.querySelectorAll('body, body *');for(var j=0;j<bodyElements.length;j++){var el=bodyElements[j];if(excludedTags.indexOf(el.tagName)>=0){continue;}var css=compileFor(el);if(css){el.setAttribute('data-computed-css',css);}}return btoa(unescape(encodeURIComponent(document.documentElement.outerHTML)));})()`

const captureCSSJS = `(function(){let c='';for(let s of document.querySelectorAll('style')){c+=(s.textContent||'')+'\n';}for(let l of document.querySelectorAll('link[href]')){let rel=(l.rel||'').toLowerCase();if(!rel.includes('stylesheet')){continue;}let h=l.href||'';if(h){c += '@import url(' + JSON.stringify(h) + ');\n';}}for(let s of document.styleSheets){try{for(let r of s.cssRules){c += r.cssText + '\n';}}catch(e){}}return btoa(unescape(encodeURIComponent(c)));})()`

const captureSafariAppleScript = `
on run argv
	if (count of argv) is not 6 then error "expected 6 args"

	set targetURLRaw to item 1 of argv
	set htmlPathRaw to item 2 of argv
	set cssPathRaw to item 3 of argv
	set pngPathRaw to item 4 of argv
	set domScriptRaw to item 5 of argv
	set cssScriptRaw to item 6 of argv

	set targetURL to (targetURLRaw as text)
	set htmlPath to (htmlPathRaw as text)
	set cssPath to (cssPathRaw as text)
	set pngPath to (pngPathRaw as text)
	set domScript to (domScriptRaw as text)
	set cssScript to (cssScriptRaw as text)

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

			set pageDOM to (do JavaScript domScript in targetTab) as Unicode text
			set pageCSS to (do JavaScript cssScript in targetTab) as Unicode text
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
	targetDir := flag.String("target-dir", "", "Directory to write capture output (required)")
	chrome := flag.Bool("chrome", false, "Force Chrome capture backend")
	safari := flag.Bool("safari", false, "Force Safari capture backend (default)")
	help := flag.Bool("help", false, "Show help and exit")
	helpShort := flag.Bool("h", false, "Show help and exit")

	flag.CommandLine.SetOutput(os.Stderr)
	flag.Usage = func() {
		program := filepath.Base(os.Args[0])
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage:\n  %s [--chrome|--safari] --target-dir <dir> <URL>\n\n", program)
		fmt.Fprintf(out, "Examples:\n  %s --target-dir captures/test https://example.com\n  %s --chrome --target-dir captures/test https://example.com\n\n", program, program)
		fmt.Fprintln(out, "Options:")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *help || *helpShort {
		flag.CommandLine.SetOutput(os.Stdout)
		flag.Usage()
		return
	}

	if strings.TrimSpace(*targetDir) == "" {
		fatalUsagef("missing required --target-dir")
	}
	if flag.NArg() != 1 {
		fatalUsagef("expected exactly one URL argument")
	}

	backend, err := resolveBackend(*chrome, *safari)
	if err != nil {
		fatalUsagef("%v", err)
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

	if err := capture(targetURL, htmlPath, cssPath, pngPath, backend); err != nil {
		fatalf("%v", err)
	}

	if err := decodeBase64CaptureFile(htmlPath); err != nil {
		fatalf("decode html: %v", err)
	}
	if err := decodeBase64CaptureFile(cssPath); err != nil {
		fatalf("decode css: %v", err)
	}

	if err := prependCSSMetadata(cssPath, targetURL, backend); err != nil {
		fatalf("annotate css: %v", err)
	}

	if err := injectStylesheetLink(htmlPath); err != nil {
		fatalf("link stylesheet: %v", err)
	}

	fmt.Printf("backend: %s\n", backend)
	fmt.Printf("saved: %s\n", htmlPath)
	fmt.Printf("saved: %s\n", cssPath)
	fmt.Printf("saved: %s\n", pngPath)
}

func resolveBackend(chrome, safari bool) (browserBackend, error) {
	if chrome && safari {
		return "", errors.New("--chrome and --safari are mutually exclusive")
	}
	if chrome {
		return backendChrome, nil
	}
	return backendSafari, nil
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

func capture(targetURL, htmlPath, cssPath, pngPath string, backend browserBackend) error {
	switch backend {
	case backendSafari:
		return captureWithSafari(targetURL, htmlPath, cssPath, pngPath)
	case backendChrome:
		return captureWithChrome(targetURL, htmlPath, cssPath, pngPath)
	default:
		return fmt.Errorf("unsupported backend: %s", backend)
	}
}

func captureDOMScript(backend browserBackend) string {
	return strings.ReplaceAll(captureDOMJS, "__CAPTURE_BROWSER__", string(backend))
}

func captureWithSafari(targetURL, htmlPath, cssPath, pngPath string) error {
	cmd := exec.Command(
		"osascript",
		"-",
		targetURL,
		htmlPath,
		cssPath,
		pngPath,
		captureDOMScript(backendSafari),
		captureCSSJS,
	)
	cmd.Stdin = strings.NewReader(captureSafariAppleScript)

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

func captureWithChrome(targetURL, htmlPath, cssPath, pngPath string) error {
	allocOptions := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	allocOptions = append(allocOptions, chromedp.WindowSize(viewportWidth, viewportHeight))
	if chromePath, ok := findChromeExecPath(); ok {
		allocOptions = append(allocOptions, chromedp.ExecPath(chromePath))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOptions...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, 120*time.Second)
	defer cancelTimeout()

	var pageDOM string
	var pageCSS string
	var screenshot []byte

	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(targetURL),
		waitForPageReady(),
		chromedp.Evaluate(captureDOMScript(backendChrome), &pageDOM),
		chromedp.Evaluate(captureCSSJS, &pageCSS),
		chromedp.CaptureScreenshot(&screenshot),
	)
	if err != nil {
		return fmt.Errorf("chromedp failed: %w", err)
	}
	if strings.TrimSpace(pageDOM) == "" {
		return errors.New("chromedp failed: empty html capture")
	}
	if len(screenshot) == 0 {
		return errors.New("chromedp failed: empty screenshot")
	}

	if err := os.WriteFile(htmlPath, []byte(pageDOM), 0o644); err != nil {
		return fmt.Errorf("write html capture: %w", err)
	}
	if err := os.WriteFile(cssPath, []byte(pageCSS), 0o644); err != nil {
		return fmt.Errorf("write css capture: %w", err)
	}
	if err := os.WriteFile(pngPath, screenshot, 0o644); err != nil {
		return fmt.Errorf("write screenshot: %w", err)
	}

	return nil
}

func waitForPageReady() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		for i := 0; i < 240; i++ {
			var readyState string
			var pageURL string

			if err := chromedp.Evaluate(`document.readyState`, &readyState).Do(ctx); err != nil {
				return err
			}
			if err := chromedp.Evaluate(`location.href || ''`, &pageURL).Do(ctx); err != nil {
				return err
			}
			if readyState == "complete" && pageURL != "" && pageURL != "about:blank" {
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		return errors.New("page did not finish loading")
	}
}

func findChromeExecPath() (string, bool) {
	macCandidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
	}
	for _, candidate := range macCandidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}

	pathCandidates := []string{"google-chrome", "chrome", "chromium", "chromium-browser"}
	for _, name := range pathCandidates {
		if resolved, err := exec.LookPath(name); err == nil {
			return resolved, true
		}
	}

	return "", false
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

func prependCSSMetadata(cssPath, sourceURL string, backend browserBackend) error {
	raw, err := os.ReadFile(cssPath)
	if err != nil {
		return err
	}

	header := fmt.Sprintf("/*\nCapture Metadata\nSource URL: %s\nBrowser: %s\n*/\n\n", sourceURL, backend)
	content := append([]byte(header), raw...)

	return os.WriteFile(cssPath, content, 0o644)
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

func fatalUsagef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n\n", args...)
	flag.Usage()
	os.Exit(2)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
