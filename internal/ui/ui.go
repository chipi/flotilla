// Package ui holds the operator UI that flotillad serves (ADR-0001: server-rendered pages, no
// separate frontend build). For now it is the stylesheet only; DESIGN.md at the repo root is its
// source, and the UI RFC adds the pages.
package ui

import "embed"

// Static is the UI's static files, served under /static/.
//
//go:embed static
var Static embed.FS
