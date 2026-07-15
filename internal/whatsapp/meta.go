package whatsapp

import (
	"context"
	"errors"
)

// MetaDriver is the pluggable Meta Cloud API (WhatsApp oficial) driver. It is a
// stub for now: selectable per tenant (tenant_channels.driver = 'meta') but not
// yet wired to send. When implemented, the official channel brings back the 24h
// service window, message templates, and — unlike Evolution — a per-message cost
// from Meta (service messages billed from 01/10/2026). That cost must be factored
// into the pricing of any tenant on the `meta` driver (Evolution has no such cost;
// the only per-resposta cost there is the AI token). See internal/whatsapp/client.go
// for the Evolution default.
type MetaDriver struct{}

// NewMetaDriver builds the (stub) Meta driver.
func NewMetaDriver() *MetaDriver { return &MetaDriver{} }

// Name identifies the Meta driver.
func (d *MetaDriver) Name() string { return "meta" }

// SendText is not implemented yet — the Meta driver is a pluggable stub. Returning
// an error here keeps the pipeline agnostic: selecting the driver never panics.
func (d *MetaDriver) SendText(_ context.Context, _, _, _ string) (string, error) {
	return "", errors.New("meta driver not configured")
}
