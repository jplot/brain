package db

const stateSchema = `
CREATE TABLE IF NOT EXISTS projects(
    id TEXT PRIMARY KEY, name TEXT NOT NULL,
    label TEXT NOT NULL CHECK(label IN ('personal','work','unsorted')),
    label_by TEXT NOT NULL CHECK(label_by IN ('rule','user')),
    updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS project_paths(
    path TEXT PRIMARY KEY, project_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS accesses(
    reader TEXT NOT NULL, target TEXT NOT NULL,
    active INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL,
    PRIMARY KEY(reader, target));
CREATE TABLE IF NOT EXISTS episodes(
    id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, start_ts TEXT NOT NULL, end_ts TEXT NOT NULL,
    title TEXT, summary TEXT, projects TEXT NOT NULL, created_at TEXT NOT NULL,
    summary_stale INTEGER NOT NULL DEFAULT 0,
    uid TEXT NOT NULL UNIQUE, updated_at TEXT NOT NULL,
    UNIQUE(session_id, start_ts));
CREATE TABLE IF NOT EXISTS points(
    id INTEGER PRIMARY KEY AUTOINCREMENT, episode_id INTEGER NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('topic','decision','pending','fact')),
    project TEXT NOT NULL, text TEXT NOT NULL,
    importance INTEGER NOT NULL CHECK(importance BETWEEN 1 AND 5),
    source_ts TEXT NOT NULL, created_at TEXT NOT NULL, closed_at TEXT,
    corrected INTEGER NOT NULL DEFAULT 0,
    uid TEXT NOT NULL UNIQUE, updated_at TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS points_fts USING fts5(
    text, content='points', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS points_ai AFTER INSERT ON points BEGIN
    INSERT INTO points_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS points_ad AFTER DELETE ON points BEGIN
    INSERT INTO points_fts(points_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS points_au AFTER UPDATE OF text ON points BEGIN
    INSERT INTO points_fts(points_fts, rowid, text) VALUES ('delete', old.id, old.text);
    INSERT INTO points_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TABLE IF NOT EXISTS forgotten_sessions(session_id TEXT PRIMARY KEY, at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS forgotten_points(uid TEXT PRIMARY KEY, at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS summary_failures(
    session_id TEXT PRIMARY KEY, attempts INTEGER NOT NULL, last_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

const indexSchema = `
CREATE TABLE IF NOT EXISTS files(
    path TEXT PRIMARY KEY, offset INTEGER NOT NULL, size INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS agent_uses(id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS sessions(
    id TEXT PRIMARY KEY, launch_cwd TEXT, launch_project TEXT, title TEXT,
    started_at TEXT, ended_at TEXT);
CREATE TABLE IF NOT EXISTS messages(
    id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL, file TEXT NOT NULL,
    ts TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('user','claude','subagent','tool')),
    project TEXT NOT NULL, git_branch TEXT, text TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    text, content='messages', content_rowid='id',
    tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
`

var indexTables = []string{"messages_fts", "messages", "sessions", "agent_uses", "files"}
