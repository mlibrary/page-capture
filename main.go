// Command page-capture captures a rendered webpage to static files.
//
// It produces three output files in a target directory:
//   - index.html     — rendered DOM with data-source-url + data-computed-css annotations
//   - styles.css     — all CSS rules (inline, @import, cssRules)
//   - screenshot.png — full-viewport screenshot
//
// Backends:
//   - chrome (default) — chromedp / headless Chrome via DevTools Protocol
//   - safari (--safari) — osascript + screencapture (macOS only)
//
// The key feature: the injected JS walks every element, computes effective
// CSS via window.getComputedStyle, and stores non-default property values
// in data-computed-css attributes. This enables offline analysis of what
// each element actually renders, independent of external stylesheets that
// won't load in a non-browser context.
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

// Viewport dimensions for the headless Chrome window.
// 1280x1200 fits most content pages with minimal wasted space.
const (
	viewportWidth  = 1280
	viewportHeight = 1200
)

// browserBackend identifies which browser engine drives the capture.
type browserBackend string

const (
	backendSafari browserBackend = "safari"
	backendChrome browserBackend = "chrome"
)

// captureDOMJS is injected into every captured page (chrome backend only).
// It does three things:
//  1. Annotates <html> with data-source-url (the actual URL) and data-browser.
//  2. Walks every visible body element and calls window.getComputedStyle(el)
//     for a curated set of CSS properties (layout, color, font, flex/grid, etc.).
//     Properties whose value matches the browser default are skipped so only
//     non-default / explicit values are recorded in data-computed-css.
//  3. Returns the full outerHTML of <html>, base64-encoded to avoid encoding
//     issues when passing through osascript or chromedp Evaluate().
//
// Default-value filters (position:static, display:inline, visibility:visible,
// overflow:visible, z-index:auto, box-sizing:content-box, 0px margins/padding,
// transparent backgrounds, etc.) keep the attribute size manageable.
const captureDOMJS = `(function(){if(!document.documentElement){return '';}var url=window.location.href||'';var browser='__CAPTURE_BROWSER__';document.documentElement.setAttribute('data-source-url',url);document.documentElement.setAttribute('data-browser',browser);var head=document.head||document.getElementsByTagName('head')[0];if(head){var meta=document.createElement('meta');meta.setAttribute('name','capture-source-url');meta.setAttribute('content',url);head.appendChild(meta);}var targetedProps=['width','height','color','background-color','display','position','margin','padding','font-size','font-family','flex-direction','grid-template-columns','opacity','visibility','z-index','box-sizing','overflow','text-align'];var excludedTags=['SCRIPT','STYLE','META','LINK','TEMPLATE','NOSCRIPT','HEAD','TITLE'];function compileFor(el){var computed=window.getComputedStyle(el);if(!computed){return '';}if(computed.getPropertyValue('display')==='none'){return '';}var out='';for(var i=0;i<targetedProps.length;i++){var prop=targetedProps[i];var value=computed.getPropertyValue(prop);if(!value){continue;}if(value==='none'||value==='normal'||value==='rgba(0, 0, 0, 0)'){continue;}if(prop==='position'&&value==='static'){continue;}if(prop==='display'&&value==='inline'){continue;}if(prop==='z-index'&&value==='auto'){continue;}if(prop==='visibility'&&value==='visible'){continue;}if((prop==='margin'||prop==='padding')&&(value==='0px'||value==='0px 0px')){continue;}if(prop==='overflow'&&value==='visible'){continue;}if(prop==='box-sizing'&&value==='content-box'){continue;}out += prop + ': ' + value + '; ';}return out.trim();}var htmlCSS=compileFor(document.documentElement);if(htmlCSS){document.documentElement.setAttribute('data-computed-css',htmlCSS);}var bodyElements=document.querySelectorAll('body, body *');for(var j=0;j<bodyElements.length;j++){var el=bodyElements[j];if(excludedTags.indexOf(el.tagName)>=0){continue;}var css=compileFor(el);if(css){el.setAttribute('data-computed-css',css);}}return btoa(unescape(encodeURIComponent(document.documentElement.outerHTML)));})()`

// captureCSSJS collects all CSS sent to the browser.
// Sources (in priority order):
//  1. <style> tags — raw textContent
//  2. <link rel="stylesheet"> tags — emitted as @import url(...)
//  3. document.styleSheets — accesses resolved cssRules (catches cross-origin
//     sheets that throw on access, those are silently skipped in catch(e){})
// Returns everything base64-encoded for safe transport through osascript.
const captureCSSJS = `(function(){let c='';for(let s of document.querySelectorAll('style')){c+=(s.textContent||'')+'\n';}for(let l of document.querySelectorAll('link[href]')){let rel=(l.rel||'').toLowerCase();if(!rel.includes('stylesheet')){continue;}let h=l.href||'';if(h){c += '@import url(' + JSON.stringify(h) + ');\n';}}for(let s of document.styleSheets){try{for(let r of s.cssRules){c += r.cssText + '\n';}}catch(e){}}return btoa(unescape(encodeURIComponent(c)));})()`

// captureSafariAppleScript is the osascript program that automates Safari.
// It receives 6 positional arguments:
//   1. target URL
//   2. output path for the HTML file (base64-encoded DOM)
//   3. output path for the CSS file (base64-encoded styles)
//   4. output path for the PNG screenshot
//   5. the DOM/JS capture script to evaluate
//   6. the CSS capture script to evaluate
//
// Workflow:
//   1. Opens Safari, navigates to target URL
//   2. Polls document.readyState until "complete" (up to 60s, 500ms intervals)
//   3. Evaluates the DOM and CSS JS scripts in the page context
//   4. Writes base64 results to HTML and CSS files via AppleScript file I/O
//   5. Uses macOS screencapture -x -R to capture the window region
//   6. Closes the Safari window
//
// The try/on-error block ensures file handles and the browser window are
// cleaned up even when capture fails.
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

// main parses flags, validates input, resolves the browser backend, and
// orchestrates the capture + post-processing pipeline.
//
// Pipeline order:
//  1. capture()                   — drive browser to get DOM, CSS, screenshot
//  2. decodeBase64CaptureFile()   — decode base64 HTML → plain text
//  3. decodeBase64CaptureFile()   — decode base64 CSS  → plain text
//  4. prependCSSMetadata()        — add source URL + backend to styles.css
//  5. injectStylesheetLink()      — add <link rel="stylesheet"> to index.html
func main() {
	targetDir := flag.String("target-dir", "", "Directory to write capture output (required)")
	chrome := flag.Bool("chrome", false, "Force Chrome capture backend (default)")
	safari := flag.Bool("safari", false, "Force Safari capture backend (Mac only)")
	help := flag.Bool("help", false, "Show help and exit")
	helpShort := flag.Bool("h", false, "Show help and exit")

	flag.CommandLine.SetOutput(os.Stderr)
	flag.Usage = func() {
		program := filepath.Base(os.Args[0])
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Usage:\n  %s [--chrome|--safari] --target-dir <dir> <URL>\n\n", program)
		fmt.Fprintf(out, "Examples:\n  %s --target-dir captures/test https://example.com\n  %s --safari --target-dir captures/test https://example.com\n\n", program, program)
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

// resolveBackend decides which browser backend to use.
// --safari overrides the default chrome; --chrome is explicit default.
// Returns an error if both are set.
func resolveBackend(chrome, safari bool) (browserBackend, error) {
	if chrome && safari {
		return "", errors.New("--chrome and --safari are mutually exclusive")
	}
	if safari {
		return backendSafari, nil
	}
	return backendChrome, nil
}

// validateURL checks that the target URL has a valid http/https scheme and host.
// url.Parse accepts many edge cases (empty strings, scheme-only), so we
// explicitly verify scheme and host presence.
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

// capture dispatches to the appropriate backend implementation.
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

// captureDOMScript returns the DOM capture JS with __CAPTURE_BROWSER__
// replaced by the actual backend name.
func captureDOMScript(backend browserBackend) string {
	return strings.ReplaceAll(captureDOMJS, "__CAPTURE_BROWSER__", string(backend))
}

// captureWithSafari drives Safari via osascript.
//
// Pipes the embedded AppleScript to osascript -, passing the target URL,
// file paths, and JS scripts as positional args. The AppleScript opens
// Safari, waits for page load, runs JS, writes files, takes a screenshot
// with screencapture, then closes the window.
//
// Error recovery uses stderr first, then stdout, then the Go exec error.
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

// captureWithChrome uses chromedp to drive headless Chrome via DevTools Protocol.
//
// Setup:
//  1. Creates an ExecAllocator with DefaultExecAllocatorOptions + window size
//  2. Tries to find a local Chrome executable
//  3. Creates a browser context with a 120s timeout
//
// Actions (single chromedp.Run batch):
//  1. Navigate to the target URL
//  2. Poll until document.readyState == "complete"
//  3. Evaluate the DOM capture JS (returns base64 outerHTML)
//  4. Evaluate the CSS capture JS (returns base64 styles)
//  5. Capture a full-viewport screenshot
//
// Writes raw results to disk; base64 decoding happens in post-processing.
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

// waitForPageReady returns a chromedp ActionFunc that polls
// document.readyState and the current URL until the page is fully
// loaded with a meaningful URL.
//
// Polls every 500ms for up to 240 iterations (120s total). Returns early
// if the context is cancelled. The "about:blank" check prevents capturing
// a blank tab that hasn't started navigation.
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

// findChromeExecPath locates a Chrome/Chromium executable on macOS.
//
// Search order:
//  1. Standard /Applications paths (Google Chrome, Chrome Canary)
//  2. PATH lookup (google-chrome, chrome, chromium, chromium-browser)
//
// Returns the path and true if found, empty string and false otherwise.
// When not found, chromedp falls back to its own discovery.
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

// decodeBase64CaptureFile reads a base64-encoded file and replaces it
// with the decoded bytes.
//
// Both the DOM and CSS capture scripts return base64 to avoid encoding
// issues through osascript or chromedp Evaluate(). This reverses that.
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

// prependCSSMetadata adds a comment header to styles.css with the source
// URL and browser backend, making it easy to trace CSS back to its origin.
func prependCSSMetadata(cssPath, sourceURL string, backend browserBackend) error {
	raw, err := os.ReadFile(cssPath)
	if err != nil {
		return err
	}

	header := fmt.Sprintf("/*\nCapture Metadata\nSource URL: %s\nBrowser: %s\n*/\n\n", sourceURL, backend)
	content := append([]byte(header), raw...)

	return os.WriteFile(cssPath, content, 0o644)
}

// injectStylesheetLink adds <link rel="stylesheet" href="styles.css"> just
// before </head> in the captured HTML. Makes the page render correctly when
// opened locally, since original external stylesheet refs won't work offline.
//
// Skips if styles.css is already linked (idempotent).
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

// fatalUsagef prints an error message, shows flag usage, and exits with code 2.
// Used for invalid CLI arguments.
func fatalUsagef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n\n", args...)
	flag.Usage()
	os.Exit(2)
}

// fatalf prints an error message to stderr and exits with code 2.
// Used for runtime errors where usage is not relevant.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
