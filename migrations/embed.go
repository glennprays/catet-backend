// Package migrations embeds the SQL files so the binary can migrate
// itself: the production image has no shell and no migrate CLI, and
// tests must apply exactly the same schema the API ships with.
package migrations

import "embed"

// FS holds every golang-migrate SQL file in this directory.
//
//go:embed *.sql
var FS embed.FS
