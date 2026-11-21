# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [v0.1.0] - 2026-11-21

First public release.

### Added

- `money`: `Amount` as int64 minor units plus ISO 4217 currency with an
  exponent table (BRL, USD, EUR, JPY, KWD, CLF, ...); overflow-checked `Add`,
  `Sub`, `Neg`, `Abs`, `MulInt`, `Cmp`; `Allocate` (largest remainder) and
  `Split`; `Parse`/`Decimal` ("1234.56") and `ParseLocale`/`Format`
  ("R$ 1.234,56") with `PtBR` and `EnUS` locales.
- `ledger`: accounts with type, normal balance and `NoOverdraft` / `Frozen`
  flags; balanced multi-leg, multi-currency transactions; idempotency keys
  with replay and conflict detection; two-phase transfers (`Hold`,
  `PostPending` with proportional partial post, `VoidPending`,
  `ExpirePending`); `Reverse` with compensating, linked transactions.
- Queries: `Balance`, `BalanceAt` (point in time), `Statement` with running
  balances and cursor pagination, `TrialBalance` with `Check`,
  `Transactions` for tailing.
- Hash-chained audit log (SHA-256 over a canonical encoding) with
  `VerifyChain`, plus `VerifyBalances` replay audit.
- `WithOnCommit` hooks for outbox relays; injectable clock and ID generator.
- `Store` interface with `memstore`, a concurrency-safe in-memory
  implementation with ordered per-account locking.
- `migrations/postgres.sql`: reference schema for a SQL store (untested).
- Concurrent simulation test asserting global invariants under `-race`;
  fuzz tests for parsing, formatting and allocation; benchmarks.
- `cmd/ledger-demo`.

[v0.1.0]: https://github.com/victorcondackdev/ledger-go/releases/tag/v0.1.0
