package store

// endedRunSQL is the SQL a message list uses for pen-gpt's stream runs that
// did not complete normally (stopped, superseded or failed): a run with
// cleanup_status <> 'not_needed', the same set pen-gpt's
// endedRunHistoryFilter hides. pen-gpt deletes such a run's assistant
// messages asynchronously; until its cleanup finishes they are still in
// mastra_messages, so every reader hides them, and the run's user message
// carries the run's status as the turn's marker.
//
// The pieces refer to the messages table as `m` and to each other by name:
//
//	with + `SELECT ..., ` + status + ` AS ended_status
//	FROM {schema}.mastra_messages m ` + join + `
//	WHERE ... ` + visible
type endedRunSQL struct {
	with    string
	join    string
	status  string
	visible string
}

// endedRuns returns the SQL for the thread named by threadParam (a
// placeholder such as "$2").
//
// The runs are the thread owner's (its resourceId), not the viewer's: a
// shared page is read by someone else. A thread without a mastra_threads
// row has no owner and so no ended runs.
//
// A message names its run by content.metadata.stream_run_id. The content is
// parsed only when the thread has an ended run, once per message: OFFSET 0
// keeps Postgres from inlining the parse into the join condition, where it
// would run once per (message, ended run) pair. The nested CASE keeps
// Postgres from casting before validating: a condition list joined with AND
// has no evaluation order, and one bad row would fail the whole page. The id
// is compared as text, so a value that is not a UUID matches nothing. A
// message without an id, or whose run is not ended, is shown.
func endedRuns(threadParam string) endedRunSQL {
	return endedRunSQL{
		with: `
		WITH ended_runs AS MATERIALIZED (
			SELECT sr.run_id::text AS run_id, sr.status
			FROM {schema}.stream_runs sr
			WHERE sr.thread_id = ` + threadParam + `
			AND sr.user_id = (SELECT "resourceId" FROM {schema}.mastra_threads WHERE id = ` + threadParam + `)
			AND sr.cleanup_status <> 'not_needed'
		)`,
		join: `
		CROSS JOIN LATERAL (
			SELECT CASE WHEN EXISTS (SELECT 1 FROM ended_runs) THEN
				CASE WHEN pg_input_is_valid(m.content, 'jsonb')
					THEN lower(m.content::jsonb #>> '{metadata,stream_run_id}')
				END
			END AS run_id
			OFFSET 0
		) message_run
		LEFT JOIN ended_runs er ON er.run_id = message_run.run_id`,
		status:  `CASE WHEN m.role = 'user' THEN er.status END`,
		visible: `AND (m.role = 'user' OR er.run_id IS NULL)`,
	}
}
