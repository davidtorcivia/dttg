-- small_key: ~400px JPEG, the smallest responsive srcset step (phones). Empty for
-- items processed before this; the view model falls back to thumb/full there.
ALTER TABLE items ADD COLUMN small_key TEXT NOT NULL DEFAULT '';
