CREATE TABLE schema_migrations (
	version TEXT PRIMARY KEY
);

CREATE TABLE users (
	id         INTEGER PRIMARY KEY,
	github_id  INTEGER NOT NULL UNIQUE,
	login      TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	avatar_url TEXT NOT NULL DEFAULT '',
	plan       TEXT NOT NULL DEFAULT 'free',
	waitlisted INTEGER NOT NULL DEFAULT 0,
	banned_at  INTEGER,
	created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX users_login_lower ON users (lower(login));

CREATE TABLE tokens (
	id           INTEGER PRIMARY KEY,
	user_id      INTEGER REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	hash         BLOB NOT NULL UNIQUE,
	prefix       TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked_at   INTEGER
);
CREATE INDEX tokens_user ON tokens (user_id);

CREATE TABLE subdomains (
	name       TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	is_default INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX subdomains_user ON subdomains (user_id);

CREATE TABLE sessions (
	id_hash    TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE usage_monthly (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	month   TEXT NOT NULL,
	bytes   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, month)
);

CREATE TABLE bans (
	kind       TEXT NOT NULL,
	value      TEXT NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (kind, value)
);

CREATE TABLE audit_log (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER,
	action     TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
