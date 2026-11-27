-- name: InsertTrainingExample :exec
-- Skipped when the project opted out of training.
insert into training_examples (project_id, issue_id, suggestion_id, signature, origin, catalog_key, model,
  prompt_version, attempt, input, output)
select @project_id, @issue_id, @suggestion_id, @signature, @origin, @catalog_key, @model,
  @prompt_version, @attempt, @input, @output
where not exists (select 1 from projects where id = @project_id and training_opt_out);

-- name: SetTrainingOutcome :exec
update training_examples set outcome = @outcome, outcome_source = @outcome_source, outcome_at = now()
where suggestion_id = @suggestion_id;

-- name: SetTrainingAttempt :exec
-- What the founder's agent changed (report_fix / "I applied the fix").
update training_examples set agent_summary = @agent_summary, files = @files
where suggestion_id = @suggestion_id;

-- name: SetProjectTrainingOptOut :exec
update projects set training_opt_out = @opt_out where id = @id;

-- name: DeleteProjectTraining :exec
delete from training_examples where project_id = $1;

-- name: TrainingExamples :many
-- For export. Ordered so the failed-then-worked pairs of one signature are adjacent.
select * from training_examples
where created_at >= @since and (sqlc.narg(outcome)::text is null or outcome = sqlc.narg(outcome))
order by signature, created_at;

-- name: TrainingCounts :one
select count(*)::bigint as total,
  count(*) filter (where outcome = 'worked')::bigint as worked,
  count(*) filter (where outcome = 'failed')::bigint as failed
from training_examples;
