package store

// messageCreatedAt is the one timestamp every message list reads, orders,
// paginates and bounds on, so a cursor.Position read from a page compares
// equal to the row it came from.
//
// "createdAtZ" is the timestamptz instant and is set on every row in both
// product schemas (checked in prod on 2026-09-30: no NULLs in public or
// windoc_mastra). The legacy "createdAt" (timestamp without time zone) is only
// a fallback; mixing it into COALESCE uncast would read it in the session
// time zone, so it is pinned to UTC, the default session zone the rows were
// written and read under.
const messageCreatedAt = `COALESCE(m."createdAtZ", m."createdAt" AT TIME ZONE 'UTC')`
