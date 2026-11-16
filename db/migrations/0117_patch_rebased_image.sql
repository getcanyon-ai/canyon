-- The image a patch run rebased (design/PATCHING.md §4.3, #2587), recorded at its step 3 before the image's scan is
-- stored, so the rescan's sweep keeps that scan while the run is pending or running: between step 3 and its deploy at
-- step 7 no deploy row names the image. Expand only: nullable, so the previous binary reads and writes runs as before.
ALTER TABLE runs ADD COLUMN rebased_image text;
