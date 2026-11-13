# Third-party notices

MazeRegistryUI is distributed under the GNU General Public License v3.0 or
later. It embeds and redistributes the components below, each under its own
licence. All of them are compatible with the GPLv3, and none of them are
covered by this project's licence.

## Bundled in the binary and shipped to the browser

### htmx 2.0.4
`web/static/htmx.min.js`
Copyright © Big Sky Software
**Zero-Clause BSD (0BSD)** — https://github.com/bigskysoftware/htmx/blob/master/LICENSE

> Permission to use, copy, modify, and/or distribute this software for any
> purpose with or without fee is hereby granted.

### Archivo
`web/static/fonts/archivo-var.woff2`
Copyright © 2016 The Archivo Project Authors (https://github.com/Omnibus-Type/Archivo)
**SIL Open Font License 1.1 (OFL-1.1)** — https://github.com/Omnibus-Type/Archivo/blob/master/OFL.txt

### JetBrains Mono
`web/static/fonts/jetbrains-mono-var.woff2`
Copyright © 2020 The JetBrains Mono Project Authors (https://github.com/JetBrains/JetBrainsMono)
**SIL Open Font License 1.1 (OFL-1.1)** — https://github.com/JetBrains/JetBrainsMono/blob/master/OFL.txt

Both files are the variable fonts' Latin subset as served by Google Fonts
(Archivo with its width and weight axes, JetBrains Mono with its weight axis).
Under the OFL they may be redistributed and embedded, including in this
binary; they may not be sold on their own.

## Linked into the binary at build time

### gopkg.in/yaml.v3 v3.0.1
Copyright © 2011-2019 Canonical Ltd, © 2006-2010 Kirill Simonov
**MIT and Apache-2.0** (the package is covered by both) —
https://github.com/go-yaml/yaml/blob/v3/LICENSE

This is the project's only Go module dependency. Everything else comes from
the Go standard library, which is covered by the BSD-3-Clause licence of the
Go project itself.

## Not redistributed

The base images referenced by the `Dockerfile` (`golang:1.25-alpine` for the
build stage, `gcr.io/distroless/static-debian12` for the runtime stage) are
pulled at build time and are not part of this repository. The published image
contains the distroless layer plus this project's statically linked binary;
consult those images for their own notices.
