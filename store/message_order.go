package store

// messageCreatedAt is the one timestamp every message list reads, orders,
// paginates and bounds on, so a cursor.Position read from a page compares
// equal to the row it came from.
//
// "createdAtZ" is the timestamptz instant and is set on every row in both
// product schemas (checked in prod on 2026-09-30: no NULLs in public or
// windoc_mastra), and Mastra's insert trigger fills it on new rows. The legacy
// "createdAt" (timestamp without time zone) is only a fallback for rows older
// than that column. It holds the writer's wall clock (8h ahead if the writer
// ran in Taipei time), and no stored value says which zone. It is read as UTC,
// the zone of pen-gpt on Cloud Run and of the database session the uncast
// COALESCE used before, so the fallback keeps the order it always had and
// does not depend on the reader's session time zone.
const messageCreatedAt = `COALESCE(m."createdAtZ", m."createdAt" AT TIME ZONE 'UTC')`
