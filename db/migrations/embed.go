// Package migrations embeds Margot's SQL migrations.
//
//   - MargotSchema: the product-global `margot` schema (applied once at boot).
//   - Tenant: per-tenant tables, applied to each tenant_<id> schema via
//     kit/tenancy.MigrationRunner (which globs *.up.sql at the FS root).
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed margot/*.sql
var margotFS embed.FS

//go:embed tenant/*.up.sql
var tenantFS embed.FS

// MargotSchema is the FS rooted at the margot/ global migrations.
var MargotSchema = mustSub(margotFS, "margot")

// Tenant is the FS rooted at the tenant/ migrations (root-level *.up.sql).
var Tenant = mustSub(tenantFS, "tenant")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
