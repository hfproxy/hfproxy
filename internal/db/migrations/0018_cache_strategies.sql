-- What the cache strategies need besides the last access. heat is a use count
-- that fades with time, as of heat_at; fill_seconds is what fetching the file
-- from the Hub took, unknown for the files already cached. A previous release
-- ignores all three.
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS heat double precision NOT NULL DEFAULT 1;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS heat_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS fill_seconds double precision;
-- The admission policies ask how often one file was requested lately.
CREATE INDEX IF NOT EXISTS downloads_file_idx ON downloads(repository_id,commit_sha,path,started_at);
