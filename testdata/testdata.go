// Package testdata embeds the shared test fixtures so any package can use
// them without locating the repo on disk.
//
// mail/ holds two lieer-shaped accounts (personal, work) plus tags.json, the
// tag manifest lieer would have applied. go:embed rejects ':' in file names, so
// maildir files are stored as "<gmail-id>!2,<flags>"; testmail.Setup renames
// them to lieer's "<gmail-id>:2,<flags>" when it materializes the fixture.
//
// hostile/ is the HTML corpus for the sanitizer and iframe assembly; its
// README.md lists what each case attacks and what correct rendering must and
// must not do.
package testdata

import "embed"

//go:embed mail hostile
var FS embed.FS
