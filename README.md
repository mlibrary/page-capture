# page-capture

`page-capture` is a Go CLI that captures a fully rendered web page and
writes three artifacts to a target directory:

- `screenshot.png` - screenshot of the rendered page
- `styles.css` - collected CSS (inline styles, linked stylesheets, and
  readable stylesheet rules)
- `index.html` - generated DOM snapshot with capture metadata

## CLI options

```text
Usage:
  page-capture [--chrome|--safari] --target-dir <dir> <URL>

Options:
  --target-dir string   Directory to write capture output (required)
  --chrome              Force Chrome capture backend
  --safari              Force Safari capture backend (default)
  --help, -h            Show help and exit
```

Notes:

- Exactly one URL argument is required.
- URL must use `http` or `https`.
- `--chrome` and `--safari` are mutually exclusive.

## What it captures

For a given URL, the tool:

1. Loads the page in a real browser backend (Headless chrome by default,
   Safari as an option on the Mac only).
2. Saves a screenshot of the rendered viewport.
3. Collects stylesheet content and writes it to `styles.css`.

- Prepends a metadata header to `styles.css` with source URL and backend.

4. Captures the generated DOM as `index.html`.

- Adds `data-source-url` to the `<html>` element.
- Adds `data-browser` to the `<html>` element (`safari` or `chrome`).
- Adds a `data-computed-css` attribute on visible elements with targeted
  computed CSS properties.
- Injects `<link rel="stylesheet" href="styles.css">` into the captured
  HTML when possible.

## Original use case

I wanted to upgrade a rails/blacklight app 
- ruby 2.78 to ruby 3.3+
- rails 5.2 to rails 8.1
- blacklight 6.15 to blacklight 9
- boostrap 3.4 to 5.3
- solr 8 to solr 10

Even after carefully walking through the upgrade and getting it 
all working, it was the (boostrap) css upgrade that caused the
most headaches, and was where I'm least experienced.

I created a list of exemplar URLs that I thought would cover
all the major pages in the application. 

I fired up two servers: one serving the original code, and one
serving the application right out of the repo on my laptop

Then I had an agent:

- Grab captures from the old system and the new system
- Compare the screenshots and look for differences to ask me about
- Follow my lead to dig into the two  `index.html` pages (which have embedded computed css)
  and `styles.css` pages to see what was going on
- Fix the (s)css in the repo 
- Capture the new-code version again, with the fix, and compare again.

Rinse and repeat until you're happy with how the new site looks/works.

For token savings, you may want to have:

- an efficient way to dig through the HTML -- [`xq`](https://github.com/sibprogrammer/xq) or the like
- a model especially good
  with images to do the screenshot comparison
- something dumber to
  rummage around and find the right bits of the original/new
  html and css to examine, and
- a smart agent to try to figure out how to
  fix it.

## Requirements

- Go 1.26+
- macOS (Safari backend uses AppleScript and `screencapture`)
- Chrome/Chromium installed if using `--chrome`

## Build

```bash
go build -o page-capture .
```

## Example use

Run with Safari backend (default):

```bash
./page-capture \
  --target-dir captures/test \
  'http://localhost:3000/m/middle-english-dictionary/dictionary?f%5Bpos%5D%5B%5D=noun&q=ab&search_field=hnf'
```

Run with Chrome backend:

```bash
./page-capture \
  --chrome \
  --target-dir captures/test \
  'http://localhost:3000/m/middle-english-dictionary/dictionary?f%5Bpos%5D%5B%5D=noun&q=ab&search_field=hnf'
```

Expected output files in `captures/test/`:

- `index.html`
- `styles.css`
- `screenshot.png`


