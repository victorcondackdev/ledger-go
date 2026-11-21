-- ledger-go: PostgreSQL schema REFERENCE.
--
-- Status: reference design, NOT exercised by this repository's tests. It
-- documents how the ledger.Store contract maps onto SQL so that a
-- pgstore implementation is mechanical. Target: PostgreSQL 14+.
--
-- Mapping of ledger.Store onto one SQL transaction (READ COMMITTED is
-- sufficient because every row that is read-modified-written is locked):
--
--   Store.Update(lock, fn)
--     BEGIN;
--     SELECT * FROM account_balances
--      WHERE account_id = ANY($1) ORDER BY account_id FOR UPDATE;   -- lock order
--     fn(tx) ...
--     COMMIT;                     -- or ROLLBACK when fn returns an error
--   StoreTx.ChainHead             SELECT seq, hash, ts FROM chain_head FOR UPDATE;
--   StoreTx.PutBalance            UPDATE account_balances SET ..., version = $v
--                                  WHERE account_id = $1 AND version = $v - k;
--   StoreTx.AppendTransaction     INSERT INTO transactions / transaction_postings /
--                                 ledger_entries; UPDATE chain_head;
--                                 unique_violation on idempotency_key
--                                 -> ledger.ErrDuplicateIdempotencyKey
--   StoreTx.PutPending            INSERT ... ON CONFLICT (id) DO UPDATE
--
-- Invariants are enforced by the ledger AND, defensively, by the database,
-- without triggers or deferred constraints:
--
--   * non-negative counters and the no-overdraft rule are CHECK constraints
--     on account_balances. They hold per row because the ledger computes
--     each account's final counters and writes them with a single UPDATE per
--     account per transaction, so no intermediate state is ever stored;
--   * one transaction per idempotency key, one reversal per transaction and
--     a gap-free, linked chain are UNIQUE / PRIMARY KEY / FOREIGN KEY
--     constraints;
--   * debits = credits per transaction is the one invariant that spans rows;
--     it is validated by the ledger before any write and audited by the
--     unbalanced_transactions view (expected empty) and trial_balance view.
--
-- Hot spot: chain_head is a single row locked by every posting. This is the
-- price of a total order with tamper evidence; see README "Design notes" for
-- the sharded-chain alternative.

BEGIN;

CREATE TABLE accounts (
    id          text        PRIMARY KEY,
    code        text        NOT NULL DEFAULT '',
    name        text        NOT NULL DEFAULT '',
    currency    char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    type        smallint    NOT NULL CHECK (type BETWEEN 1 AND 5),      -- asset..expense
    normal      smallint    NOT NULL CHECK (normal IN (1, 2)),          -- 1 debit, 2 credit
    flags       integer     NOT NULL DEFAULT 0 CHECK (flags >= 0),      -- 1 no_overdraft, 2 frozen
    created_at  timestamptz NOT NULL
);

-- One row per account, updated in the same SQL transaction as the entries.
-- normal and no_overdraft are copied from accounts (both immutable) so the
-- overdraft rule can be a plain CHECK constraint.
CREATE TABLE account_balances (
    account_id       text     PRIMARY KEY REFERENCES accounts (id),
    normal           smallint NOT NULL CHECK (normal IN (1, 2)),
    no_overdraft     boolean  NOT NULL,
    debits_posted    bigint   NOT NULL DEFAULT 0 CHECK (debits_posted   >= 0),
    credits_posted   bigint   NOT NULL DEFAULT 0 CHECK (credits_posted  >= 0),
    debits_pending   bigint   NOT NULL DEFAULT 0 CHECK (debits_pending  >= 0),
    credits_pending  bigint   NOT NULL DEFAULT 0 CHECK (credits_pending >= 0),
    version          bigint   NOT NULL DEFAULT 0 CHECK (version >= 0),  -- = number of entries
    -- Available balance (posted minus outgoing holds) never negative.
    CONSTRAINT no_overdraft CHECK (
        NOT no_overdraft
        OR (normal = 2 AND credits_posted - debits_posted - debits_pending >= 0)
        OR (normal = 1 AND debits_posted - credits_posted - credits_pending >= 0)
    )
);

-- The audit chain: one row per ledger transaction, append-only.
CREATE TABLE transactions (
    seq              bigint      PRIMARY KEY CHECK (seq > 0),
    id               text        NOT NULL UNIQUE,                        -- UUIDv7
    kind             smallint    NOT NULL CHECK (kind BETWEEN 1 AND 6),
    idempotency_key  text        UNIQUE,                                 -- NULL: none
    ref              text        REFERENCES transactions (id),
    ts               timestamptz NOT NULL,
    expires_at       timestamptz,
    description      text        NOT NULL DEFAULT '',
    metadata         jsonb       NOT NULL DEFAULT '{}',
    request_hash     bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    prev_hash        bytea       NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash             bytea       NOT NULL UNIQUE CHECK (octet_length(hash) = 32),
    -- Linking: every row but the first points at its predecessor's hash.
    CONSTRAINT ref_required CHECK ((kind IN (1, 2)) = (ref IS NULL)),
    CONSTRAINT expiry_only_on_pending CHECK (expires_at IS NULL OR kind = 2)
);

-- A transaction can be reversed once (kind 6 = reversal).
CREATE UNIQUE INDEX transactions_one_reversal ON transactions (ref) WHERE kind = 6;
-- A pending transfer is resolved once (post / void / expire).
CREATE UNIQUE INDEX transactions_one_resolution ON transactions (ref) WHERE kind IN (3, 4, 5);

-- Postings in request order; the canonical hash covers them in this order.
CREATE TABLE transaction_postings (
    seq         bigint   NOT NULL REFERENCES transactions (seq),
    ordinal     smallint NOT NULL CHECK (ordinal >= 0 AND ordinal < 128),
    account_id  text     NOT NULL REFERENCES accounts (id),
    direction   smallint NOT NULL CHECK (direction IN (1, 2)),
    currency    char(3)  NOT NULL,
    amount      bigint   NOT NULL CHECK (amount > 0),
    PRIMARY KEY (seq, ordinal)
);

-- Per-account effects with the counters after each one: statements with
-- running balances and point-in-time balances are index range scans.
CREATE TABLE ledger_entries (
    account_id       text        NOT NULL REFERENCES accounts (id),
    account_seq      bigint      NOT NULL CHECK (account_seq > 0),
    seq              bigint      NOT NULL REFERENCES transactions (seq),
    kind             smallint    NOT NULL,
    ts               timestamptz NOT NULL,
    direction        smallint    NOT NULL CHECK (direction IN (1, 2)),
    phase            smallint    NOT NULL CHECK (phase IN (1, 2, 3)),   -- posted, held, released
    amount           bigint      NOT NULL CHECK (amount > 0),
    debits_posted    bigint      NOT NULL,
    credits_posted   bigint      NOT NULL,
    debits_pending   bigint      NOT NULL,
    credits_pending  bigint      NOT NULL,
    PRIMARY KEY (account_id, account_seq)          -- Entries(after, limit)
);
CREATE INDEX ledger_entries_by_time ON ledger_entries (account_id, ts, account_seq);  -- EntryAt

-- State machine of two-phase transfers; the pending transaction itself
-- stays immutable.
CREATE TABLE pending_transfers (
    id           text        PRIMARY KEY REFERENCES transactions (id),
    status       smallint    NOT NULL CHECK (status BETWEEN 1 AND 4),   -- open, posted, voided, expired
    expires_at   timestamptz,
    resolved_by  text        REFERENCES transactions (id),
    CONSTRAINT resolved_iff_closed CHECK ((status = 1) = (resolved_by IS NULL))
);
CREATE INDEX pending_transfers_due ON pending_transfers (expires_at, id)
    WHERE status = 1 AND expires_at IS NOT NULL;                         -- ExpiredPending

-- Single-row chain tip, locked FOR UPDATE by every append.
CREATE TABLE chain_head (
    singleton  boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    seq        bigint      NOT NULL DEFAULT 0,
    hash       bytea       NOT NULL DEFAULT decode(repeat('00', 32), 'hex'),
    ts         timestamptz NOT NULL DEFAULT 'epoch'
);
INSERT INTO chain_head DEFAULT VALUES;

-- History is append-only: the application role may only INSERT/SELECT.
-- REVOKE UPDATE, DELETE ON transactions, transaction_postings, ledger_entries FROM ledger_app;

-- Audit views. Both must be empty / zero in a healthy ledger.
CREATE VIEW unbalanced_transactions AS
SELECT seq, currency,
       sum(amount) FILTER (WHERE direction = 1) AS debits,
       sum(amount) FILTER (WHERE direction = 2) AS credits
  FROM transaction_postings
 GROUP BY seq, currency
HAVING coalesce(sum(amount) FILTER (WHERE direction = 1), 0)
    <> coalesce(sum(amount) FILTER (WHERE direction = 2), 0);

CREATE VIEW trial_balance AS
SELECT a.currency,
       sum(b.debits_posted)   AS debits_posted,
       sum(b.credits_posted)  AS credits_posted,
       sum(b.debits_pending)  AS debits_pending,
       sum(b.credits_pending) AS credits_pending,
       sum(b.debits_posted) - sum(b.credits_posted)   AS posted_difference,   -- must be 0
       sum(b.debits_pending) - sum(b.credits_pending) AS pending_difference   -- must be 0
  FROM account_balances b
  JOIN accounts a ON a.id = b.account_id
 GROUP BY a.currency;

COMMIT;
