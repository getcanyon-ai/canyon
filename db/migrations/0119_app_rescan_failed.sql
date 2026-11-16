-- When and why the last match of an image's app layers that deps.known-vuln alone reads did not finish
-- (design/PATCHING.md §3.2, §3.6; #2355): it marks only that rule's read of the built app, never the platform
-- read's rescan_failed_at, so a failed app layer never stops the platform rule or the patch plan. Expand only:
-- columns the previous binary never reads.
ALTER TABLE scanned_images
  ADD COLUMN app_rescan_failed_at timestamptz,
  ADD COLUMN app_rescan_failed    text;
