-- AI judgement is offered on gate.unreadable only (#1327): auth.unauthenticated-route's findings are
-- the AI reviewer's own, and Canyon's AI approves nothing. A mode an earlier Canyon kept for that rule
-- stays as written (the server reads gate.unreadable's alone, so it moves no snapshot); from here the
-- table refuses one, as the route does.
ALTER TABLE judgement_edits DROP CONSTRAINT judgement_edits_check;
ALTER TABLE judgement_edits ADD CONSTRAINT judgement_edits_check CHECK (rule = 'gate.unreadable' AND mode IN ('advise', 'off')) NOT VALID;
