CREATE TABLE custom_domains (
	host        TEXT NOT NULL,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token       TEXT NOT NULL,
	verified_at INTEGER,
	created_at  INTEGER NOT NULL,
	PRIMARY KEY (host, user_id)
);
-- Many users may have a pending row for a host; only one may verify it.
CREATE UNIQUE INDEX custom_domains_verified ON custom_domains(host) WHERE verified_at IS NOT NULL;
CREATE INDEX custom_domains_user ON custom_domains(user_id);
