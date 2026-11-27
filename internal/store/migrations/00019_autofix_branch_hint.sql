-- +goose Up

-- Auto-fix runs that fail because the code isn't where the daemon looked.
--   projects.autofix_branch: the branch auto-fix works from, set on the
--     Setup page ('' = what the daemon was given with fixassist add).
--   autofix_runs.hint: the founder's note for this run ("it's in
--     Controllers/LearningErrors.cs"), put in the agent's prompt.
--   autofix_runs.fail_reason: why it failed, when we know: 'not_in_repo'
--     (the agent didn't find the code that raises the error).
alter table projects add column autofix_branch text not null default '';
alter table autofix_runs add column hint text not null default '';
alter table autofix_runs add column fail_reason text not null default '';

-- +goose Down
alter table autofix_runs drop column fail_reason;
alter table autofix_runs drop column hint;
alter table projects drop column autofix_branch;
