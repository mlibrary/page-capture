# page-capture

`page-capture` is a Go CLI that captures a fully rendered web page and writes three artifacts to a target directory:

- `screenshot.png` - screenshot of the rendered page
- `styles.css` - collected CSS (inline styles, linked stylesheets, and readable stylesheet rules)
- `index.html` - generated DOM snapshot with capture metadata


## What it captures

For a given URL, the tool:

1. Loads the page in a real browser backend (Safari by default, or Chrome).
2. Saves a screenshot of the rendered viewport.
3. Collects stylesheet content and writes it to `styles.css`.
    - Prepends a metadata header to `styles.css` with source URL and backend.
4. Captures the generated DOM as `index.html`. 
     - Adds `data-source-url` to the `<html>` element.
     - Adds `data-browser` to the `<html>` element (`safari` or `chrome`).
     - Adds a `data-computed-css` attribute on visible elements with targeted computed CSS properties.
     - Injects `<link rel="stylesheet" href="styles.css">` into the captured HTML when possible.

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

## Development reference data

This repo includes reference capture outputs from the original script in:

- `captures/reference/`

You can compare generated outputs in `captures/test/` against those files.
