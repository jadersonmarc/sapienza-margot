// Package gating answers, read-only, whether a user may operate a product for a
// tenant at some tier. It reads only the control-plane schema (public:
// memberships, subscriptions, plans) and never writes — the golden rule that
// only sapienza-core mutates public. A test asserts this package issues no
// write statements.
package gating

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Client reads gating facts from public.
type Client struct {
	pool *pgxpool.Pool
}

// New builds a gating client over the given pool.
func New(pool *pgxpool.Pool) *Client {
	return &Client{pool: pool}
}

// Access describes a user's relationship to a product within a tenant.
type Access struct {
	Member       bool   // user belongs to the tenant
	Role         string // owner|admin|member (empty if not a member)
	Subscribed   bool   // tenant has a subscription for the product
	Tier         string // subscription tier (empty if not subscribed)
	Status       string // subscription status (active|past_due|canceled|...)
	HardCap      bool   // plan enforces a hard usage cap
}

// Lookup returns the full access picture for (user, tenant, produto).
func (c *Client) Lookup(ctx context.Context, userID, tenantID uuid.UUID, produto string) (Access, error) {
	var a Access

	err := c.pool.QueryRow(ctx,
		`SELECT role FROM public.memberships WHERE user_id = $1 AND tenant_id = $2`,
		userID, tenantID,
	).Scan(&a.Role)
	switch {
	case err == pgx.ErrNoRows:
		// not a member; leave a zero-valued
	case err != nil:
		return a, fmt.Errorf("lookup membership: %w", err)
	default:
		a.Member = true
	}

	err = c.pool.QueryRow(ctx,
		`SELECT tier, status, COALESCE(hard_cap, false)
		   FROM public.subscriptions
		  WHERE tenant_id = $1 AND produto = $2`,
		tenantID, produto,
	).Scan(&a.Tier, &a.Status, &a.HardCap)
	switch {
	case err == pgx.ErrNoRows:
		// no subscription
	case err != nil:
		return a, fmt.Errorf("lookup subscription: %w", err)
	default:
		a.Subscribed = true
	}

	return a, nil
}

// CanOperate reports whether the user may operate the product now: they must be
// a member of the tenant and the tenant's subscription must be active.
func (c *Client) CanOperate(ctx context.Context, userID, tenantID uuid.UUID, produto string) (bool, error) {
	a, err := c.Lookup(ctx, userID, tenantID, produto)
	if err != nil {
		return false, err
	}
	return a.Member && a.Subscribed && a.Status == "active", nil
}

// TenantAccess describes a tenant's subscription to a product, independent of any
// user. Data planes use it to gate work triggered without a user in context
// (e.g. an inbound WhatsApp webhook): the tenant must have an active subscription.
type TenantAccess struct {
	Subscribed bool
	Status     string // active|past_due|canceled|... (empty if not subscribed)
	Tier       string
	HardCap    bool
}

// TenantSubscription returns the tenant's subscription facts for a product.
func (c *Client) TenantSubscription(ctx context.Context, tenantID uuid.UUID, produto string) (TenantAccess, error) {
	var a TenantAccess
	err := c.pool.QueryRow(ctx,
		`SELECT tier, status, COALESCE(hard_cap, false)
		   FROM public.subscriptions
		  WHERE tenant_id = $1 AND produto = $2`,
		tenantID, produto,
	).Scan(&a.Tier, &a.Status, &a.HardCap)
	switch {
	case err == pgx.ErrNoRows:
		return a, nil
	case err != nil:
		return a, fmt.Errorf("lookup tenant subscription: %w", err)
	}
	a.Subscribed = true
	return a, nil
}

// TenantCanOperate reports whether the tenant's product subscription is active.
func (c *Client) TenantCanOperate(ctx context.Context, tenantID uuid.UUID, produto string) (bool, error) {
	a, err := c.TenantSubscription(ctx, tenantID, produto)
	if err != nil {
		return false, err
	}
	return a.Subscribed && a.Status == "active", nil
}
