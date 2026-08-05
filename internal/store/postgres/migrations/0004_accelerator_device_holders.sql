-- 0004 — persist agent-observed device holders per accelerator report
-- (see the SQLite 0013 notes). The nil-vs-empty distinction is load-bearing:
-- JSON 'null' means the agent did not look, '[]' means it looked and found
-- nothing. The 'null' default keeps pre-existing rows "not observed".
ALTER TABLE accelerator_reports ADD COLUMN holders_json TEXT NOT NULL DEFAULT 'null';
