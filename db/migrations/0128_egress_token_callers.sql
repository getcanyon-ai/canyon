-- Token callers on Kubernetes (design/EGRESS.md §4.3, #2142).
--
-- token_only is the server's mark on an app env: set once a /run whose pods carry canyon-identity's forwarder
-- succeeded with its earlier pods gone, never by a pod or an executor's POST /callers answer, and never by a failed
-- /run. With it the policy names the env's pods by their token alone, never by their address. Every env starts
-- without it: a pod deployed before its forwarder existed is still its address's until its env's next deploy.
-- Setting it moves seq, as every write to egress_envs does (0096).
ALTER TABLE egress_envs ADD COLUMN token_only boolean NOT NULL DEFAULT false;

-- kube_keys is the fingerprint of the cluster's issuer and key set the last answer named, NULL on Docker or before
-- one was named: another moves seq, so no replica is told 304 for a version under another key set.
ALTER TABLE egress_policy ADD COLUMN kube_keys text;
