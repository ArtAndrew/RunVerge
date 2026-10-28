-- SPDX-License-Identifier: Elastic-2.0
-- A paid attempt refers to the task's original goal. A new goal must be a new
-- task (or a future explicit revision protocol), not an in-place edit.
CREATE FUNCTION immutable_task_goal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.goal IS DISTINCT FROM OLD.goal THEN
  RAISE EXCEPTION 'task goal is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_goal_immutable BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION immutable_task_goal();

-- The admission clock and fence are a receipt, not an extendable lease. The
-- first pending->sent transition may populate them; later transitions preserve
-- the original values even when an outcome becomes unknown or confirmed.
CREATE FUNCTION immutable_dispatch_permit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.dispatch_issued_at IS NOT NULL AND (
  NEW.dispatch_issued_at IS DISTINCT FROM OLD.dispatch_issued_at OR
  NEW.dispatch_not_after IS DISTINCT FROM OLD.dispatch_not_after OR
  NEW.dispatch_fencing_token IS DISTINCT FROM OLD.dispatch_fencing_token
 ) THEN
  RAISE EXCEPTION 'dispatch permit is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER dispatch_permit_immutable BEFORE UPDATE ON actions FOR EACH ROW EXECUTE FUNCTION immutable_dispatch_permit();
