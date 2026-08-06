package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS oryxa_sessions (
  id          TEXT PRIMARY KEY,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seq    BIGINT      NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS oryxa_events (
  session_id  TEXT        NOT NULL REFERENCES oryxa_sessions(id) ON DELETE CASCADE,
  seq         BIGINT      NOT NULL,
  ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
  kind        TEXT        NOT NULL,
  actor       TEXT        NOT NULL DEFAULT '',
  turn        TEXT        NOT NULL DEFAULT '',
  data        JSONB,
  PRIMARY KEY (session_id, seq)
);

CREATE INDEX IF NOT EXISTS oryxa_events_session_seq
  ON oryxa_events (session_id, seq);
`

type pgLog struct {
	pool *pgxpool.Pool
	fan  *fanout
}

// NewPostgres opens the pool, applies the schema, and returns a durable store.
func NewPostgres(ctx context.Context, dsn string) (Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &pgLog{pool: pool, fan: newFanout()}, nil
}

// Append allocates the next sequence number and writes the event in one
// transaction.
//
// The sequence comes from a counter row rather than MAX(seq)+1: the UPDATE takes
// a row lock, so concurrent appends to one session serialise instead of racing
// for the same number. That lock is per session, so different rooms never block
// each other — which mirrors the runtime, where each session is already serial
// and different sessions are not.
func (l *pgLog) Append(sessionID, kind, actor, turn string, data any) (Event, error) {
	raw, err := encode(data)
	if err != nil {
		return Event{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var ev Event
	err = pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		var seq int64
		err := tx.QueryRow(ctx, `
			INSERT INTO oryxa_sessions (id, last_seq) VALUES ($1, 1)
			ON CONFLICT (id) DO UPDATE SET last_seq = oryxa_sessions.last_seq + 1
			RETURNING last_seq`, sessionID).Scan(&seq)
		if err != nil {
			return fmt.Errorf("allocate seq: %w", err)
		}

		var ts time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO oryxa_events (session_id, seq, kind, actor, turn, data)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING ts`,
			sessionID, seq, kind, actor, turn, raw).Scan(&ts)
		if err != nil {
			return fmt.Errorf("insert event: %w", err)
		}

		ev = Event{
			Seq: seq, Session: sessionID, TS: ts.UTC(),
			Kind: kind, Actor: actor, Turn: turn, Data: raw,
		}
		return nil
	})
	if err != nil {
		return Event{}, err
	}

	// Only after the commit. Publishing earlier would let a subscriber see an
	// event that a rolled-back transaction never actually wrote.
	l.fan.publish(ev)
	return ev, nil
}

func (l *pgLog) Since(sessionID string, since int64) ([]Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := l.pool.Query(ctx, `
		SELECT seq, ts, kind, actor, turn, data
		FROM oryxa_events WHERE session_id = $1 AND seq > $2
		ORDER BY seq`, sessionID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		ev := Event{Session: sessionID}
		var data []byte
		if err := rows.Scan(&ev.Seq, &ev.TS, &ev.Kind, &ev.Actor, &ev.Turn, &data); err != nil {
			return nil, err
		}
		ev.TS = ev.TS.UTC()
		if len(data) > 0 {
			ev.Data = json.RawMessage(data)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (l *pgLog) Subscribe(sessionID string) (<-chan Event, func()) {
	return l.fan.subscribe(sessionID)
}

func (l *pgLog) Sessions() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := l.pool.Query(ctx,
		`SELECT id FROM oryxa_sessions ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (l *pgLog) Close() error {
	l.pool.Close()
	return nil
}
