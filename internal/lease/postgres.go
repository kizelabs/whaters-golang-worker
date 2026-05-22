package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Coordinator interface {
	DB() *sql.DB
	Acquire(ctx context.Context, sessionID string) (int64, bool, error)
	Renew(ctx context.Context, sessionID string, token int64) (bool, error)
	Release(ctx context.Context, sessionID string, token int64) error
	GetSessionDeviceJID(ctx context.Context, sessionID string) (string, error)
	UpsertSessionDeviceJID(ctx context.Context, sessionID, deviceJID string) error
	DeleteSessionDeviceJID(ctx context.Context, sessionID string) error
}

type PostgresCoordinator struct {
	db         *sql.DB
	instanceID string
	ttl        time.Duration
	grace      time.Duration
	schema     string
}

func NewPostgresCoordinator(databaseURL, databaseSchema, instanceID string, ttl, grace time.Duration) (*PostgresCoordinator, error) {
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required when SESSION_STORE_DRIVER=postgres")
	}
	if instanceID == "" {
		return nil, errors.New("INSTANCE_ID is required when SESSION_STORE_DRIVER=postgres")
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	schema, err := sanitizeIdentifier(databaseSchema)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	c := &PostgresCoordinator{
		db:         db,
		instanceID: instanceID,
		ttl:        ttl,
		grace:      grace,
		schema:     schema,
	}
	if err := c.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *PostgresCoordinator) DB() *sql.DB {
	return c.db
}

func (c *PostgresCoordinator) ensureSchema(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, fmt.Sprintf(`
CREATE SCHEMA IF NOT EXISTS %s;
CREATE TABLE IF NOT EXISTS %s.wa_session_leases (
  session_id TEXT PRIMARY KEY,
  owner_instance_id TEXT NOT NULL,
  fenced_token BIGINT NOT NULL DEFAULT 1,
  lease_expires_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`, c.schema, c.schema))
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.wa_session_devices (
  session_id TEXT PRIMARY KEY,
  device_jid TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`, c.schema))
	return err
}

func (c *PostgresCoordinator) Acquire(ctx context.Context, sessionID string) (int64, bool, error) {
	stmt := fmt.Sprintf(`
WITH upsert AS (
	INSERT INTO %s.wa_session_leases (session_id, owner_instance_id, fenced_token, lease_expires_at, updated_at)
	VALUES ($1, $2, 1, NOW() + (($3 + $4) * INTERVAL '1 second'), NOW())
	ON CONFLICT (session_id) DO UPDATE SET
		owner_instance_id = CASE
			WHEN %s.wa_session_leases.owner_instance_id = EXCLUDED.owner_instance_id
				OR %s.wa_session_leases.lease_expires_at <= NOW()
			THEN EXCLUDED.owner_instance_id
			ELSE %s.wa_session_leases.owner_instance_id
		END,
		fenced_token = CASE
			WHEN %s.wa_session_leases.owner_instance_id = EXCLUDED.owner_instance_id
				OR %s.wa_session_leases.lease_expires_at <= NOW()
			THEN %s.wa_session_leases.fenced_token + 1
			ELSE %s.wa_session_leases.fenced_token
		END,
		lease_expires_at = CASE
			WHEN %s.wa_session_leases.owner_instance_id = EXCLUDED.owner_instance_id
				OR %s.wa_session_leases.lease_expires_at <= NOW()
			THEN NOW() + (($3 + $4) * INTERVAL '1 second')
			ELSE %s.wa_session_leases.lease_expires_at
		END,
		updated_at = NOW()
	RETURNING owner_instance_id, fenced_token
)
SELECT owner_instance_id, fenced_token FROM upsert`, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema, c.schema)

	var owner string
	var token int64
	if err := c.db.QueryRowContext(ctx, stmt, sessionID, c.instanceID, int(c.ttl.Seconds()), int(c.grace.Seconds())).Scan(&owner, &token); err != nil {
		return 0, false, err
	}
	return token, owner == c.instanceID, nil
}

func (c *PostgresCoordinator) Renew(ctx context.Context, sessionID string, token int64) (bool, error) {
	result, err := c.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s.wa_session_leases
SET lease_expires_at = NOW() + (($1 + $2) * INTERVAL '1 second'),
    updated_at = NOW()
WHERE session_id = $3
  AND owner_instance_id = $4
  AND fenced_token = $5`, c.schema), int(c.ttl.Seconds()), int(c.grace.Seconds()), sessionID, c.instanceID, token)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (c *PostgresCoordinator) Release(ctx context.Context, sessionID string, token int64) error {
	_, err := c.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE %s.wa_session_leases
SET lease_expires_at = NOW() - INTERVAL '1 second',
    updated_at = NOW()
WHERE session_id = $1
  AND owner_instance_id = $2
  AND fenced_token = $3`, c.schema), sessionID, c.instanceID, token)
	return err
}

func (c *PostgresCoordinator) GetSessionDeviceJID(ctx context.Context, sessionID string) (string, error) {
	var jid string
	err := c.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT device_jid FROM %s.wa_session_devices WHERE session_id = $1`, c.schema), sessionID).Scan(&jid)
	return jid, err
}

func (c *PostgresCoordinator) UpsertSessionDeviceJID(ctx context.Context, sessionID, deviceJID string) error {
	_, err := c.db.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s.wa_session_devices (session_id, device_jid, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (session_id) DO UPDATE
SET device_jid = EXCLUDED.device_jid,
    updated_at = NOW()`, c.schema), sessionID, deviceJID)
	return err
}

func (c *PostgresCoordinator) DeleteSessionDeviceJID(ctx context.Context, sessionID string) error {
	_, err := c.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s.wa_session_devices WHERE session_id = $1`, c.schema), sessionID)
	return err
}

func sanitizeIdentifier(value string) (string, error) {
	if value == "" {
		return "public", nil
	}
	if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(value) {
		return "", errors.New("DATABASE_SCHEMA must be a valid SQL identifier")
	}
	return value, nil
}
