-- Refined-variant fields populated by the M2 ingest pipeline.
-- thumb_key: small JPEG used in the grid; cover_key remains the full variant.
-- placeholder: a tiny base64 JPEG data-URI for blur-up while the image loads.
ALTER TABLE items ADD COLUMN thumb_key   TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN placeholder TEXT NOT NULL DEFAULT '';
