CREATE TABLE subscriptions (
	user_id         INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	customer_id     TEXT NOT NULL UNIQUE,
	subscription_id TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT '',
	event_created   INTEGER NOT NULL DEFAULT 0,
	updated_at      INTEGER NOT NULL
);

CREATE TABLE stripe_events (
	id          TEXT PRIMARY KEY,
	type        TEXT NOT NULL,
	received_at INTEGER NOT NULL
);
