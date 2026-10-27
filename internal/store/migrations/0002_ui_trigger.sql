-- Runs started from the web console have their own trigger value.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check CHECK (trigger IN ('api', 'github', 'ui'));
