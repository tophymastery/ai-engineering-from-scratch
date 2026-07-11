-- unregistered.sql — the RED fixture for the "unregistered-table ⇒ CI red"
-- criterion. It introduces a NEW table with a raw-PII column that is NOT listed
-- in services/identity-profile/data-inventory.yaml. tools/piiscan check-inventory
-- MUST flag it and exit nonzero.
CREATE TABLE IF NOT EXISTS marketing_leads (
    lead_id    TEXT NOT NULL PRIMARY KEY,
    full_name  TEXT NOT NULL,           -- pii:name    (UNREGISTERED — must fail CI)
    home_email TEXT                     -- pii:email   (UNREGISTERED — must fail CI)
);
