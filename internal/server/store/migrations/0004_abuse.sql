CREATE TABLE abuse_reports (
	id          INTEGER PRIMARY KEY,
	url         TEXT NOT NULL,
	category    TEXT NOT NULL,
	details     TEXT NOT NULL,
	email       TEXT NOT NULL DEFAULT '',
	ip          TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	resolved_at INTEGER
);
CREATE INDEX abuse_reports_open ON abuse_reports(created_at) WHERE resolved_at IS NULL;
