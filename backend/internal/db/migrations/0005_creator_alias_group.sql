-- Migration v5 — contract v1.4: creator display alias + grouping.
--
-- creators.alias       user-set display name (NULL = show nickname as-is).
-- creators.group_name  grouping label for the library tree (NULL = ungrouped).
-- The column is named group_name because GROUP is a SQL keyword.

ALTER TABLE creators ADD COLUMN alias TEXT NULL;
ALTER TABLE creators ADD COLUMN group_name TEXT NULL;
