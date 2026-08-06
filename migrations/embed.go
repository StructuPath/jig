// Package migrations embeds the numbered control-plane schema migrations.
package migrations

import "embed"

// Files contains the numbered .sql migrations, applied in filename order.
//
//go:embed *.sql
var Files embed.FS
