// Package store holds the database helpers shared by services.
// Every service connects to its OWN database; nothing here is shared data.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"

	"library/pkg/events"
)

// Open connects to Postgres (retrying until it is up) and runs the schema.
func Open(dsn string, schema ...string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	for attempt := 1; attempt <= 30; attempt++ {
		if err = db.Ping(); err == nil {
			break
		}
		log.Printf("[db] not ready (attempt %d): %v", attempt, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	// Every consumer records which events it already processed (idempotency).
	schema = append(schema, outboxSchema, `CREATE TABLE IF NOT EXISTS processed_events (
		event_id     TEXT PRIMARY KEY,
		event_type   TEXT NOT NULL,
		processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("schema: %w\n%s", err, stmt)
		}
	}
	return db, nil
}

// TxHandler does a consumer's work inside a transaction.
type TxHandler func(ctx context.Context, tx *sql.Tx, env events.Envelope) error

// Idempotent wraps a handler so that the same eventId is processed at most once.
// The "already processed?" check, the business change and any follow-up events
// (written to the outbox) all commit in ONE transaction, so a crash in between can
// never leave the event half-applied.
func Idempotent(db *sql.DB, h TxHandler) events.Handler {
	return func(ctx context.Context, env events.Envelope) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck

		res, err := tx.ExecContext(ctx,
			`INSERT INTO processed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			env.EventID, env.EventType)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			log.Printf("[idempotency] duplicate %s id=%s ignored", env.EventType, env.EventID)
			return nil
		}
		if err := h(ctx, tx, env); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("[consumer] processed %s id=%s from %s", env.EventType, env.EventID, env.Source)
		return nil
	}
}

// ---- Transactional outbox ----
// Services never publish directly from a request or handler. They write the event
// to their own outbox table in the SAME transaction as the data change. A relay
// then publishes it. If RabbitMQ is down, events wait in the outbox instead of
// being lost; once it is back, the relay catches up.

const outboxSchema = `CREATE TABLE IF NOT EXISTS outbox (
	id           BIGSERIAL PRIMARY KEY,
	event_id     TEXT NOT NULL UNIQUE,
	event_type   TEXT NOT NULL,
	envelope     JSONB NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	published_at TIMESTAMPTZ
)`

// Enqueue builds an event and stores it in the outbox inside tx.
func Enqueue(ctx context.Context, tx *sql.Tx, source, eventType string, payload any) (events.Envelope, error) {
	env, err := events.New(eventType, source, payload)
	if err != nil {
		return env, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return env, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO outbox (event_id, event_type, envelope) VALUES ($1, $2, $3)`,
		env.EventID, env.EventType, body)
	return env, err
}

// RunOutboxRelay publishes pending outbox rows forever.
func RunOutboxRelay(db *sql.DB, b *events.Broker) {
	for {
		if n, err := relayOnce(db, b); err != nil {
			log.Printf("[outbox] relay error: %v", err)
			time.Sleep(2 * time.Second)
		} else if n == 0 {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func relayOnce(db *sql.DB, b *events.Broker) (int, error) {
	rows, err := db.Query(`SELECT id, envelope FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT 50`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id  int64
		env events.Envelope
	}
	var items []item
	for rows.Next() {
		var it item
		var raw []byte
		if err := rows.Scan(&it.id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal(raw, &it.env); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if err := b.Publish(context.Background(), it.env); err != nil {
			return 0, err // stays unpublished; retried on the next pass
		}
		if _, err := db.Exec(`UPDATE outbox SET published_at = now() WHERE id = $1`, it.id); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}
