package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCursorSchema holds per-consumer cursors, kept out of public so
// products never write to public for bookkeeping. The core creates it.
const DefaultCursorSchema = "bus"

// Publish appends an event to public.event_outbox inside the caller's
// transaction (transactional outbox) and fires a pg_notify on commit so
// listeners wake promptly. payload is marshaled to JSON.
//
// tx must be the same transaction as the state change being recorded, so the
// event is durably linked to it (committed together or not at all).
func Publish(ctx context.Context, tx pgx.Tx, typ Type, tenantID uuid.UUID, produto string, payload any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO public.event_outbox (type, tenant_id, produto, payload)
		 VALUES ($1, $2, NULLIF($3, ''), $4)
		 RETURNING id`,
		string(typ), tenantID, produto, body,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert event_outbox: %w", err)
	}
	// pg_notify is transactional: the notification is delivered on commit.
	if _, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, NotifyChannel, fmt.Sprint(id)); err != nil {
		return 0, fmt.Errorf("notify: %w", err)
	}
	return id, nil
}

// Consumer reads events from the outbox in id order, tracking progress in a
// per-consumer cursor row (schema "bus"). At-least-once delivery: a consumer
// should make handling idempotent.
type Consumer struct {
	pool         *pgxpool.Pool
	name         string
	cursorSchema string
}

// NewConsumer builds a consumer identified by name (its cursor key).
func NewConsumer(pool *pgxpool.Pool, name string) *Consumer {
	return &Consumer{pool: pool, name: name, cursorSchema: DefaultCursorSchema}
}

// WithCursorSchema overrides the schema holding event_cursors (default "bus").
func (c *Consumer) WithCursorSchema(schema string) *Consumer {
	c.cursorSchema = schema
	return c
}

func (c *Consumer) cursorTable() string {
	return pgx.Identifier{c.cursorSchema, "event_cursors"}.Sanitize()
}

// Cursor returns the last acknowledged event id for this consumer (0 if none).
func (c *Consumer) Cursor(ctx context.Context) (int64, error) {
	var last int64
	err := c.pool.QueryRow(ctx,
		`SELECT last_id FROM `+c.cursorTable()+` WHERE consumer = $1`, c.name,
	).Scan(&last)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read cursor %q: %w", c.name, err)
	}
	return last, nil
}

// Fetch returns up to limit events with id greater than the current cursor,
// in ascending id order. It does not advance the cursor — call Ack after
// successfully handling them.
func (c *Consumer) Fetch(ctx context.Context, limit int) ([]Envelope, error) {
	last, err := c.Cursor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx,
		`SELECT id, type, tenant_id, COALESCE(produto, ''), payload, created_at
		   FROM public.event_outbox
		  WHERE id > $1
		  ORDER BY id
		  LIMIT $2`, last, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fetch events: %w", err)
	}
	defer rows.Close()

	var out []Envelope
	for rows.Next() {
		var e Envelope
		var raw []byte
		if err := rows.Scan(&e.ID, &e.Type, &e.TenantID, &e.Produto, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &e.Payload); err != nil {
				return nil, fmt.Errorf("unmarshal event %d payload: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ack advances the consumer cursor to upTo (the highest handled event id).
func (c *Consumer) Ack(ctx context.Context, upTo int64) error {
	_, err := c.pool.Exec(ctx,
		`INSERT INTO `+c.cursorTable()+` (consumer, last_id)
		 VALUES ($1, $2)
		 ON CONFLICT (consumer) DO UPDATE SET last_id = EXCLUDED.last_id, updated_at = now()`,
		c.name, upTo,
	)
	if err != nil {
		return fmt.Errorf("ack cursor %q: %w", c.name, err)
	}
	return nil
}

// DecodePayload unmarshals an envelope's payload into a typed struct.
func DecodePayload[T any](e Envelope) (T, error) {
	var v T
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v)
	return v, err
}
