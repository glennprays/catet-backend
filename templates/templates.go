// Package templates embeds the repository's template files so the
// packages that render them never depend on the process working
// directory — the binary carries its copy with it.
package templates

import "embed"

// FS holds the bot reply templates, one file per supported language.
//
//go:embed bot/*.tmpl
var FS embed.FS
