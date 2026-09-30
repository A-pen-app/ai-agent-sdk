package store

// endedRunMessageFilter hides the non-user messages of a pen-gpt stream run
// that did not complete normally (stopped, superseded or failed). pen-gpt
// deletes them asynchronously; until its cleanup finishes they are still in
// mastra_messages, so every reader filters them the same way pen-gpt's
// endedRunHistoryFilter does: a run with cleanup_status <> 'not_needed'.
//
// The run is named by content.metadata.stream_run_id. The nested CASE keeps
// Postgres from casting before validating: a condition list joined with AND
// has no evaluation order, and one bad row would fail the whole page. A
// message without a valid id, or whose run is not found, is shown.
//
// The run must belong to the thread's owner (its resourceId), not to the
// viewer: a shared page is read by someone else. The query using it must
// LEFT JOIN {schema}.mastra_threads t ON t.id = m.thread_id.
const endedRunMessageFilter = `
		AND NOT (
			m.role <> 'user'
			AND EXISTS (
				SELECT 1 FROM {schema}.stream_runs sr
				WHERE sr.run_id = (
					CASE WHEN pg_input_is_valid(m.content, 'jsonb') THEN
						CASE WHEN pg_input_is_valid(m.content::jsonb #>> '{metadata,stream_run_id}', 'uuid')
							THEN (m.content::jsonb #>> '{metadata,stream_run_id}')::uuid
						END
					END
				)
				AND sr.thread_id = m.thread_id
				AND sr.user_id = t."resourceId"
				AND sr.cleanup_status <> 'not_needed'
			)
		)
`
