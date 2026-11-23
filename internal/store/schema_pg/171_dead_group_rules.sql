-- Schema step 171: hand-offs to groups that no longer exist.
--
-- Deleting a group used to leave any programme handed to it in place.
-- Such a rule grants nobody, but settings showed "Nobody else" while it
-- stayed stored. Removing it changes who can do what for no one.
DELETE FROM corp_permissions p
WHERE p.kind = 'group'
  AND NOT EXISTS (SELECT 1 FROM org_groups g WHERE g.id::text = p.ref);

DELETE FROM owner_permissions p
WHERE p.kind = 'group'
  AND NOT EXISTS (SELECT 1 FROM org_groups g WHERE g.id::text = p.ref);
