-- Store 5-swatch perceptual OKLab palettes (comma-separated #rrggbb hexes)
-- on both individual gallery plates and parent items (aggregated across plates
-- for multi-image series).
ALTER TABLE items ADD COLUMN palette TEXT NOT NULL DEFAULT '';
ALTER TABLE item_images ADD COLUMN palette TEXT NOT NULL DEFAULT '';
