package skilld

import "embed"

// WebAssets embeds the static frontend UI files into the single binary.
//
//go:embed packages/web/index.html packages/web/styles.css packages/web/app.mjs
var WebAssets embed.FS

// MigrationFiles embeds all ordered SQL schema migrations into the single binary.
//
//go:embed migrations/*.sql
var MigrationFiles embed.FS
