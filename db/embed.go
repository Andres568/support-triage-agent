// Package db embeds the SQL migrations so cmd/migrate ships them inside the
// binary: the prod migration job needs no files next to it.
package db

import "embed"

// Migrations holds migrations/*.sql, the same files the goose CLI applies in dev.
//
//go:embed migrations/*.sql
var Migrations embed.FS
