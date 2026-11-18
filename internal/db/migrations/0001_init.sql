-- Initial schema. All timestamps are INTEGER unix seconds.

-- Members are keyed on Slack user ID (plan KTD3): forge-proxy has no user
-- row for someone until their first sign-in, and imported Slack authors
-- may never sign in. forge_user_id is attached when they first arrive.
CREATE TABLE members (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    slack_user_id TEXT    NOT NULL UNIQUE,
    forge_user_id INTEGER,
    name          TEXT    NOT NULL,
    avatar_url    TEXT    NOT NULL DEFAULT '',
    dm_replies    INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL,
    last_seen_at  INTEGER
);

CREATE TABLE tags (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    slug       TEXT    NOT NULL UNIQUE,
    active     INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

INSERT INTO tags (slug, created_at) VALUES
    ('general', unixepoch()),
    ('ai', unixepoch()),
    ('architecture', unixepoch()),
    ('go', unixepoch()),
    ('kubernetes', unixepoch()),
    ('data-engineering', unixepoch()),
    ('forge-cloud', unixepoch()),
    ('careers', unixepoch());

CREATE TABLE threads (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    author_id        INTEGER NOT NULL REFERENCES members(id),
    title            TEXT    NOT NULL,
    body_md          TEXT    NOT NULL,
    score            INTEGER NOT NULL DEFAULT 0,
    reply_count      INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    edited_at        INTEGER,
    last_activity_at INTEGER NOT NULL,
    deleted_at       INTEGER,
    removed_by       INTEGER REFERENCES members(id),
    -- Set only for threads moved from Slack.
    slack_channel_id   TEXT,
    slack_channel_name TEXT,
    slack_thread_ts    TEXT,
    slack_permalink    TEXT,
    slack_remaining    INTEGER,
    moved_by           INTEGER REFERENCES members(id)
);

-- A Slack thread can be moved once (plan R20).
CREATE UNIQUE INDEX threads_slack_source
    ON threads (slack_channel_id, slack_thread_ts)
    WHERE slack_channel_id IS NOT NULL;

CREATE INDEX threads_activity ON threads (last_activity_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX threads_score ON threads (score DESC, created_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE thread_tags (
    thread_id INTEGER NOT NULL REFERENCES threads(id),
    tag_id    INTEGER NOT NULL REFERENCES tags(id),
    PRIMARY KEY (thread_id, tag_id)
);

CREATE INDEX thread_tags_tag ON thread_tags (tag_id);

CREATE TABLE replies (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    thread_id  INTEGER NOT NULL REFERENCES threads(id),
    parent_id  INTEGER REFERENCES replies(id),
    author_id  INTEGER NOT NULL REFERENCES members(id),
    body_md    TEXT    NOT NULL,
    score      INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    edited_at  INTEGER,
    deleted_at INTEGER,
    removed_by INTEGER REFERENCES members(id),
    imported   INTEGER NOT NULL DEFAULT 0,
    slack_ts   TEXT
);

CREATE INDEX replies_thread ON replies (thread_id, parent_id, created_at);

CREATE TABLE votes (
    member_id   INTEGER NOT NULL REFERENCES members(id),
    target_kind TEXT    NOT NULL CHECK (target_kind IN ('thread', 'reply')),
    target_id   INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (member_id, target_kind, target_id)
);

-- Outbox for Slack messages (plan KTD6). Rows are written in the same
-- transaction as the post that causes them and delivered by a worker.
CREATE TABLE notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT    NOT NULL CHECK (kind IN ('announce', 'reply_dm', 'link_back', 'move_failed')),
    payload         TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'sent', 'failed', 'skipped')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    sent_at         INTEGER
);

CREATE INDEX notifications_pending ON notifications (next_attempt_at) WHERE status = 'pending';
